package axis_airquality

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/inputs"
)

//go:embed sample.conf
var sampleConfig string

const (
	defaultLookback = config.Duration(5 * time.Minute)
	defaultTimeout  = config.Duration(10 * time.Second)

	sensorsPath = "/config/rest/airqualitymonitor/v1beta/sensors"
	serialPath  = "/axis-cgi/param.cgi?action=list&group=Properties.System.SerialNumber"

	modeHistory = "history"
	modeStream  = "stream"
)

// eventKeyToField maps the data keys published by the
// tnsaxis:AirQualityMonitor/Metadata event to metric field names. These keys
// differ from the REST history API category names.
var eventKeyToField = map[string]string{
	"Temperature": "temperature",
	"Humidity":    "humidity",
	"CO2":         "co2",
	"NOx":         "nox",
	"PM1.0":       "pm1_0",
	"PM2.5":       "pm2_5",
	"PM4.0":       "pm4_0",
	"PM10.0":      "pm10_0",
	"VOC":         "voc",
	"AQI":         "aqi",
	"HeatIndex":   "heat_index",
	"Humidex":     "humidex",
}

// categoryToField maps Air Quality monitor API categories to metric field names.
var categoryToField = map[string]string{
	"TEMPERATURE": "temperature",
	"HUMIDITY":    "humidity",
	"CO2":         "co2",
	"NOX":         "nox",
	"PM1_0":       "pm1_0",
	"PM2_5":       "pm2_5",
	"PM4_0":       "pm4_0",
	"PM10_0":      "pm10_0",
	"VOC":         "voc",
	"AQI":         "aqi",
	"HEAT_INDEX":  "heat_index",
	"HUMIDEX":     "humidex",
}

// Ensure the plugin satisfies the streaming service input contract. The execd
// shim only drives Start/Stop when this interface matches, so the assertion
// guards stream mode against accidental signature drift.
var _ telegraf.ServiceInput = (*AxisAirQuality)(nil)

// AxisAirQuality is a Telegraf input plugin that collects measurements from an
// Axis air quality sensor (e.g. AXIS D6310) through the VAPIX Air Quality
// monitor API.
type AxisAirQuality struct {
	URL        string          `toml:"url"`
	Username   config.Secret   `toml:"username"`
	Password   config.Secret   `toml:"password"`
	Mode       string          `toml:"mode"`
	SensorID   string          `toml:"sensor_id"`
	Serial     string          `toml:"serial"`
	Categories []string        `toml:"categories"`
	Lookback   config.Duration `toml:"lookback"`
	Timeout    config.Duration `toml:"timeout"`

	InsecureSkipVerify bool `toml:"insecure_skip_verify"`

	Log telegraf.Logger `toml:"-"`

	client  *http.Client
	baseURL string

	// Resolved device serial number, cached after the first successful lookup.
	serial string

	// Stored for the websocket digest handshake used by stream mode.
	user string
	pass string

	// Streaming lifecycle state (stream mode only).
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type sensorsResponse struct {
	Status string `json:"status"`
	Data   []struct {
		Connected     bool   `json:"connected"`
		DefaultSensor bool   `json:"defaultSensor"`
		SensorID      string `json:"sensorId"`
		Type          string `json:"type"`
	} `json:"data"`
}

type sensorInfo struct {
	Connected bool
	SensorID  string
	Type      string
}

type historyResponse struct {
	Status string `json:"status"`
	Data   struct {
		Measurement []float64 `json:"measurement"`
		Timestamp   []int64   `json:"timestamp"`
	} `json:"data"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (*AxisAirQuality) SampleConfig() string {
	return sampleConfig
}

func (a *AxisAirQuality) Init() error {
	if a.URL == "" {
		return errors.New("\"url\" is required")
	}
	a.baseURL = strings.TrimRight(a.URL, "/")

	if a.Mode == "" {
		a.Mode = modeStream
	}
	a.Mode = strings.ToLower(a.Mode)
	if a.Mode != modeHistory && a.Mode != modeStream {
		return fmt.Errorf("unsupported mode %q, expected %q or %q", a.Mode, modeHistory, modeStream)
	}

	if a.Lookback <= 0 {
		a.Lookback = defaultLookback
	}
	if a.Timeout <= 0 {
		a.Timeout = defaultTimeout
	}
	if len(a.Categories) == 0 {
		a.Categories = []string{
			"TEMPERATURE", "HUMIDITY", "CO2", "NOX",
			"PM1_0", "PM2_5", "PM4_0", "PM10_0", "VOC", "AQI",
		}
	}
	for _, c := range a.Categories {
		if _, ok := categoryToField[strings.ToUpper(c)]; !ok {
			return fmt.Errorf("unsupported category %q", c)
		}
	}

	username, err := a.Username.Get()
	if err != nil {
		return fmt.Errorf("getting username failed: %w", err)
	}
	defer username.Destroy()
	password, err := a.Password.Get()
	if err != nil {
		return fmt.Errorf("getting password failed: %w", err)
	}
	defer password.Destroy()

	a.user = username.String()
	a.pass = password.String()

	a.client = &http.Client{
		Timeout: time.Duration(a.Timeout),
		Transport: newDigestTransport(
			a.user,
			a.pass,
			a.InsecureSkipVerify,
		),
	}

	return nil
}

// Start begins real-time collection when running in stream mode. In history
// mode it is a no-op and collection happens via Gather.
func (a *AxisAirQuality) Start(acc telegraf.Accumulator) error {
	if a.Mode != modeStream {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.streamLoop(ctx, acc)
	}()
	return nil
}

// Stop terminates the streaming goroutine. It is safe to call when not
// streaming.
func (a *AxisAirQuality) Stop() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
}

func (a *AxisAirQuality) Gather(acc telegraf.Accumulator) error {
	// In stream mode metrics are pushed asynchronously from Start.
	if a.Mode == modeStream {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()

	sensorID := a.SensorID
	sensorType := ""
	connected := true

	if sensor, err := a.defaultSensor(ctx); err != nil {
		// If a sensor_id is explicitly configured, continue without discovery.
		if sensorID == "" {
			return err
		}
		a.Log.Debugf("sensor discovery failed, using configured sensor_id %q: %v", sensorID, err)
	} else {
		if sensorID == "" {
			sensorID = sensor.SensorID
		}
		sensorType = sensor.Type
		connected = sensor.Connected
	}

	if sensorID == "" {
		return errors.New("no sensor found and no sensor_id configured")
	}

	now := time.Now()
	start := now.Add(-time.Duration(a.Lookback))

	fields := make(map[string]interface{})
	var latest time.Time

	for _, category := range a.Categories {
		category = strings.ToUpper(category)
		value, ts, ok, err := a.latestMeasurement(ctx, sensorID, category, start, now)
		if err != nil {
			acc.AddError(fmt.Errorf("reading category %q failed: %w", category, err))
			continue
		}
		if !ok {
			continue
		}
		fields[categoryToField[category]] = value
		if ts.After(latest) {
			latest = ts
		}
	}

	if len(fields) == 0 {
		return nil
	}

	tags := map[string]string{
		"source":    a.host(),
		"sensor_id": sensorID,
	}
	if sensorType != "" {
		tags["sensor_type"] = sensorType
	}
	if serial := a.resolveSerial(ctx); serial != "" {
		tags["serial"] = serial
	}
	fields["connected"] = connected

	if latest.IsZero() {
		latest = now
	}
	acc.AddFields("axis_airquality", fields, tags, latest)

	return nil
}

func (a *AxisAirQuality) defaultSensor(ctx context.Context) (sensorInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+sensorsPath, nil)
	if err != nil {
		return sensorInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	body, err := a.do(req)
	if err != nil {
		return sensorInfo{}, err
	}

	var resp sensorsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return sensorInfo{}, fmt.Errorf("decoding sensors response failed: %w", err)
	}
	if resp.Status != "success" || len(resp.Data) == 0 {
		return sensorInfo{}, errors.New("no sensors reported by device")
	}

	// Prefer the default sensor, otherwise take the first one.
	chosen := resp.Data[0]
	for _, s := range resp.Data {
		if s.DefaultSensor {
			chosen = s
			break
		}
	}
	return sensorInfo{
		Connected: chosen.Connected,
		SensorID:  chosen.SensorID,
		Type:      chosen.Type,
	}, nil
}

// resolveSerial returns the device serial number used as the stable `serial`
// tag. A configured value takes precedence; otherwise it is fetched from the
// device once and cached. Lookup failures are non-fatal and yield an empty
// string so the tag is simply omitted.
func (a *AxisAirQuality) resolveSerial(ctx context.Context) string {
	if a.serial != "" {
		return a.serial
	}
	if a.Serial != "" {
		a.serial = a.Serial
		return a.serial
	}
	serial, err := a.deviceSerial(ctx)
	if err != nil {
		a.Log.Debugf("serial discovery failed: %v", err)
		return ""
	}
	a.serial = serial
	return serial
}

// deviceSerial fetches the serial number from the device parameter API. The
// response is a single `Properties.System.SerialNumber=<serial>` line.
func (a *AxisAirQuality) deviceSerial(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+serialPath, nil)
	if err != nil {
		return "", err
	}

	body, err := a.do(req)
	if err != nil {
		return "", err
	}

	line := strings.TrimSpace(string(body))
	_, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", fmt.Errorf("unexpected serial response %q", line)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("device reported empty serial number")
	}
	return value, nil
}

func (a *AxisAirQuality) latestMeasurement(
	ctx context.Context,
	sensorID, category string,
	start, end time.Time,
) (float64, time.Time, bool, error) {
	payload := map[string]interface{}{
		"data": map[string]interface{}{
			"category":  category,
			"startTime": start.Unix(),
			"endTime":   end.Unix(),
		},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return 0, time.Time{}, false, err
	}

	url := fmt.Sprintf("%s%s/%s/getHistoryData", a.baseURL, sensorsPath, sensorID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return 0, time.Time{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	body, err := a.do(req)
	if err != nil {
		return 0, time.Time{}, false, err
	}

	var resp historyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, time.Time{}, false, fmt.Errorf("decoding history response failed: %w", err)
	}
	if resp.Status != "success" {
		if resp.Error != nil {
			return 0, time.Time{}, false, fmt.Errorf("device error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return 0, time.Time{}, false, fmt.Errorf("unexpected status %q", resp.Status)
	}

	n := len(resp.Data.Measurement)
	if n == 0 {
		return 0, time.Time{}, false, nil
	}
	value := resp.Data.Measurement[n-1]
	ts := end
	if len(resp.Data.Timestamp) == n {
		ts = time.Unix(resp.Data.Timestamp[n-1], 0)
	}
	return value, ts, true, nil
}

func (a *AxisAirQuality) do(req *http.Request) ([]byte, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (a *AxisAirQuality) host() string {
	if u, err := url.Parse(a.baseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return a.baseURL
}

func init() {
	inputs.Add("axis_airquality", func() telegraf.Input {
		return &AxisAirQuality{
			Lookback: defaultLookback,
			Timeout:  defaultTimeout,
		}
	})
}
