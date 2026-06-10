package axis_airquality

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/testutil"
	"github.com/stretchr/testify/require"
)

// digestServer wraps a test handler with a one-shot digest challenge so the
// digest transport is exercised end to end.
func digestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Digest realm="AXIS_TEST", nonce="abc123", algorithm=MD5, qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Digest ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
}

func newTestPlugin(t *testing.T, url string) *AxisAirQuality {
	t.Helper()
	plugin := &AxisAirQuality{
		URL:      url,
		Username: config.NewSecret([]byte("root")),
		Password: config.NewSecret([]byte("secret")),
		Log:      testutil.Logger{},
	}
	require.NoError(t, plugin.Init())
	return plugin
}

func TestInitDefaults(t *testing.T) {
	plugin := newTestPlugin(t, "http://10.0.0.10/")
	require.Equal(t, "http://10.0.0.10", plugin.baseURL)
	require.Equal(t, defaultLookback, plugin.Lookback)
	require.Equal(t, defaultTimeout, plugin.Timeout)
	require.NotEmpty(t, plugin.Categories)
}

func TestInitRequiresURL(t *testing.T) {
	plugin := &AxisAirQuality{
		Username: config.NewSecret([]byte("root")),
		Password: config.NewSecret([]byte("secret")),
	}
	require.Error(t, plugin.Init())
}

func TestInitRejectsUnknownCategory(t *testing.T) {
	plugin := &AxisAirQuality{
		URL:        "http://10.0.0.10",
		Categories: []string{"NONSENSE"},
		Username:   config.NewSecret([]byte("root")),
		Password:   config.NewSecret([]byte("secret")),
	}
	require.ErrorContains(t, plugin.Init(), "unsupported category")
}

func TestInitDefaultsToStreamMode(t *testing.T) {
	plugin := newTestPlugin(t, "http://10.0.0.10")
	require.Equal(t, modeStream, plugin.Mode)
}

func TestInitRejectsBadMode(t *testing.T) {
	plugin := &AxisAirQuality{
		URL:      "http://10.0.0.10",
		Mode:     "realtime",
		Username: config.NewSecret([]byte("root")),
		Password: config.NewSecret([]byte("secret")),
	}
	require.ErrorContains(t, plugin.Init(), "unsupported mode")
}

func TestStreamModeGatherIsNoop(t *testing.T) {
	plugin := &AxisAirQuality{
		URL:      "http://10.0.0.10",
		Mode:     "stream",
		Username: config.NewSecret([]byte("root")),
		Password: config.NewSecret([]byte("secret")),
		Log:      testutil.Logger{},
	}
	require.NoError(t, plugin.Init())

	var acc testutil.Accumulator
	require.NoError(t, plugin.Gather(&acc))
	require.Empty(t, acc.GetTelegrafMetrics())
}

func TestAllowedFields(t *testing.T) {
	plugin := &AxisAirQuality{Categories: []string{"TEMPERATURE", "co2", "AQI"}}
	allowed := plugin.allowedFields()
	require.True(t, allowed["temperature"])
	require.True(t, allowed["co2"])
	require.True(t, allowed["aqi"])
	require.False(t, allowed["humidity"])
}

func TestResolveSerialUsesConfiguredValue(t *testing.T) {
	plugin := &AxisAirQuality{Serial: "MYSERIAL123", Log: testutil.Logger{}}
	require.Equal(t, "MYSERIAL123", plugin.resolveSerial(context.Background()))
}

func TestDeviceSerial(t *testing.T) {
	server := digestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.RawQuery, "Properties.System.SerialNumber")
		_, _ = io.WriteString(w, "Properties.System.SerialNumber=E827251A7B8B\n")
	})
	defer server.Close()

	plugin := &AxisAirQuality{
		URL:      server.URL,
		Username: config.NewSecret([]byte("root")),
		Password: config.NewSecret([]byte("secret")),
		Log:      testutil.Logger{},
	}
	require.NoError(t, plugin.Init())

	require.Equal(t, "E827251A7B8B", plugin.resolveSerial(context.Background()))
	// Second call is served from cache without another request.
	require.Equal(t, "E827251A7B8B", plugin.resolveSerial(context.Background()))
}

func TestEmitEvent(t *testing.T) {
	plugin := &AxisAirQuality{
		Categories: []string{"TEMPERATURE", "CO2", "AQI"},
		baseURL:    "http://10.0.0.10",
		Log:        testutil.Logger{},
	}
	var note wsNotification
	note.Method = "events:notify"
	note.Params.Notification.Topic = airQualityTopic
	note.Params.Notification.Timestamp = 1781077634960
	note.Params.Notification.Message.Source = map[string]string{"sensor_name": "D6310"}
	note.Params.Notification.Message.Data = map[string]string{
		"Temperature": "20.5",
		"CO2":         "433",
		"AQI":         "1",
		"Humidity":    "33.9", // not in configured categories, should be dropped
	}

	var acc testutil.Accumulator
	plugin.emitEvent(&acc, note, plugin.allowedFields(), "0", "D6310", "E827251AA4C6")

	m, ok := acc.Get("axis_airquality")
	require.True(t, ok)
	require.Equal(t, "0", m.Tags["sensor_id"])
	require.Equal(t, "D6310", m.Tags["sensor_type"])
	require.Equal(t, "E827251AA4C6", m.Tags["serial"])
	require.InDelta(t, 20.5, m.Fields["temperature"], 1e-9)
	require.InDelta(t, 433.0, m.Fields["co2"], 1e-9)
	require.InDelta(t, 1.0, m.Fields["aqi"], 1e-9)
	require.NotContains(t, m.Fields, "humidity")
	require.Equal(t, true, m.Fields["connected"])
	require.Equal(t, time.Unix(0, 1781077634960*int64(time.Millisecond)), m.Time)
}

func TestStreamOnce(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probe request (no Upgrade header) -> issue digest challenge.
		if r.Header.Get("Upgrade") == "" {
			w.Header().Set("WWW-Authenticate",
				`Digest realm="AXIS_TEST", nonce="abc123", algorithm=MD5, qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Expect the subscribe message.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		notify := `{"apiVersion":"1.0","method":"events:notify","params":{"notification":{"topic":"tnsaxis:AirQualityMonitor/Metadata","timestamp":1781077634960,"message":{"source":{"sensor_name":"D6310"},"data":{"CO2":"433","Temperature":"20.5","AQI":"1"}}}}}`
		for {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(notify)); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer server.Close()

	plugin := &AxisAirQuality{
		URL:        server.URL,
		Mode:       "stream",
		Categories: []string{"TEMPERATURE", "CO2", "AQI"},
		Username:   config.NewSecret([]byte("root")),
		Password:   config.NewSecret([]byte("secret")),
		Log:        testutil.Logger{},
	}
	require.NoError(t, plugin.Init())

	var acc testutil.Accumulator
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = plugin.streamOnce(ctx, &acc, "0", "D6310", "E827251AA4C6") }()

	acc.Wait(1)
	cancel()

	m, ok := acc.Get("axis_airquality")
	require.True(t, ok)
	require.Equal(t, "D6310", m.Tags["sensor_type"])
	require.Equal(t, "E827251AA4C6", m.Tags["serial"])
	require.InDelta(t, 433.0, m.Fields["co2"], 1e-9)
	require.InDelta(t, 20.5, m.Fields["temperature"], 1e-9)
}

func TestGather(t *testing.T) {
	server := digestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "param.cgi") {
			_, _ = io.WriteString(w, "Properties.System.SerialNumber=E827251AA4C6\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sensors"):
			_, _ = io.WriteString(w, `{"status":"success","data":[{"connected":true,"defaultSensor":true,"sensorId":"0","type":"D6310"}]}`)
		case strings.HasSuffix(r.URL.Path, "/getHistoryData"):
			var req struct {
				Data struct {
					Category string `json:"category"`
				} `json:"data"`
			}
			body, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(body, &req))
			value := 1.5
			if req.Data.Category == "CO2" {
				value = 429
			}
			resp := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"measurement": []float64{value - 1, value},
					"timestamp":   []int64{1781076299, 1781076359},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()

	plugin := &AxisAirQuality{
		URL:        server.URL,
		Mode:       "history",
		Categories: []string{"TEMPERATURE", "CO2"},
		Username:   config.NewSecret([]byte("root")),
		Password:   config.NewSecret([]byte("secret")),
		Log:        testutil.Logger{},
	}
	require.NoError(t, plugin.Init())

	var acc testutil.Accumulator
	require.NoError(t, plugin.Gather(&acc))
	require.Empty(t, acc.Errors)

	require.True(t, acc.HasMeasurement("axis_airquality"))
	m, ok := acc.Get("axis_airquality")
	require.True(t, ok)

	require.Equal(t, "0", m.Tags["sensor_id"])
	require.Equal(t, "D6310", m.Tags["sensor_type"])
	require.Equal(t, "E827251AA4C6", m.Tags["serial"])
	require.InDelta(t, 1.5, m.Fields["temperature"], 1e-9)
	require.InDelta(t, 429.0, m.Fields["co2"], 1e-9)
	require.Equal(t, true, m.Fields["connected"])
	require.Equal(t, time.Unix(1781076359, 0), m.Time)
}

func TestGatherExplicitSensorID(t *testing.T) {
	server := digestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/sensors") {
			// Simulate discovery failure.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.Contains(r.URL.Path, "/sensors/7/getHistoryData") {
			_, _ = io.WriteString(w, `{"status":"success","data":{"measurement":[20.5],"timestamp":[1781076359]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer server.Close()

	plugin := &AxisAirQuality{
		URL:        server.URL,
		SensorID:   "7",
		Mode:       "history",
		Categories: []string{"TEMPERATURE"},
		Username:   config.NewSecret([]byte("root")),
		Password:   config.NewSecret([]byte("secret")),
		Log:        testutil.Logger{},
	}
	require.NoError(t, plugin.Init())

	var acc testutil.Accumulator
	require.NoError(t, plugin.Gather(&acc))
	m, ok := acc.Get("axis_airquality")
	require.True(t, ok)
	require.Equal(t, "7", m.Tags["sensor_id"])
	require.InDelta(t, 20.5, m.Fields["temperature"], 1e-9)
}

func TestSplitParams(t *testing.T) {
	params := splitParams(`realm="AXIS_TEST", nonce="a,b,c", algorithm=MD5, qop="auth"`)
	require.Equal(t, "AXIS_TEST", params["realm"])
	require.Equal(t, "a,b,c", params["nonce"])
	require.Equal(t, "MD5", params["algorithm"])
	require.Equal(t, "auth", params["qop"])
}

func TestParseChallenge(t *testing.T) {
	c := parseChallenge(`Digest realm="r", nonce="n", qop="auth", opaque="o"`)
	require.NotNil(t, c)
	require.Equal(t, "r", c.realm)
	require.Equal(t, "n", c.nonce)
	require.Equal(t, "auth", c.qop)
	require.Equal(t, "o", c.opaque)

	require.Nil(t, parseChallenge(""))
	require.Nil(t, parseChallenge("Basic realm=\"r\""))
}
