package axis_airquality

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/influxdata/telegraf"
)

const (
	eventStreamPath = "/vapix/ws-data-stream?sources=events"
	airQualityTopic = "tnsaxis:AirQualityMonitor/Metadata"

	streamReconnectMin = 1 * time.Second
	streamReconnectMax = 30 * time.Second
)

// wsNotification models the events:notify message pushed by the Axis event
// websocket stream.
type wsNotification struct {
	Method string `json:"method"`
	Params struct {
		Notification struct {
			Topic     string `json:"topic"`
			Timestamp int64  `json:"timestamp"` // milliseconds since epoch
			Message   struct {
				Source map[string]string `json:"source"`
				Data   map[string]string `json:"data"`
			} `json:"message"`
		} `json:"notification"`
	} `json:"params"`
}

// streamLoop maintains a websocket subscription to the air quality metadata
// event and pushes a metric for every notification. It reconnects with backoff
// until the context is cancelled.
func (a *AxisAirQuality) streamLoop(ctx context.Context, acc telegraf.Accumulator) {
	// Resolve sensor identity once for tagging. Failure here is non-fatal.
	sensorID := a.SensorID
	sensorType := ""
	if sensor, err := a.defaultSensor(ctx); err != nil {
		a.Log.Debugf("sensor discovery failed in stream mode: %v", err)
	} else {
		if sensorID == "" {
			sensorID = sensor.SensorID
		}
		sensorType = sensor.Type
	}

	backoff := streamReconnectMin
	for {
		if ctx.Err() != nil {
			return
		}

		err := a.streamOnce(ctx, acc, sensorID, sensorType)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			acc.AddError(fmt.Errorf("air quality event stream: %w", err))
		}

		// Backoff before reconnecting.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < streamReconnectMax {
			backoff *= 2
			if backoff > streamReconnectMax {
				backoff = streamReconnectMax
			}
		}
	}
}

// streamOnce establishes a single websocket connection, subscribes, and reads
// notifications until an error or context cancellation.
func (a *AxisAirQuality) streamOnce(ctx context.Context, acc telegraf.Accumulator, sensorID, sensorType string) error {
	conn, err := a.dialEventStream(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Subscribe to the air quality metadata topic only.
	subscribe := fmt.Sprintf(
		`{"apiVersion":"1.0","method":"events:configure","params":{"eventFilterList":[{"topicFilter":%q}]}}`,
		airQualityTopic,
	)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(subscribe)); err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}

	// Close the connection when the context is cancelled to unblock ReadMessage.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	allowed := a.allowedFields()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read failed: %w", err)
		}

		var note wsNotification
		if err := json.Unmarshal(raw, &note); err != nil {
			a.Log.Debugf("skipping unparseable event message: %v", err)
			continue
		}
		if note.Method != "events:notify" {
			continue
		}
		if note.Params.Notification.Topic != airQualityTopic {
			continue
		}

		a.emitEvent(acc, note, allowed, sensorID, sensorType)
	}
}

func (a *AxisAirQuality) emitEvent(
	acc telegraf.Accumulator,
	note wsNotification,
	allowed map[string]bool,
	sensorID, sensorType string,
) {
	data := note.Params.Notification.Message.Data
	fields := make(map[string]interface{}, len(data))
	for key, raw := range data {
		field, ok := eventKeyToField[key]
		if !ok || !allowed[field] {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			a.Log.Debugf("skipping non-numeric value for %q: %q", key, raw)
			continue
		}
		fields[field] = value
	}
	if len(fields) == 0 {
		return
	}
	fields["connected"] = true

	tags := map[string]string{
		"source":    a.host(),
		"sensor_id": sensorID,
	}
	// The event carries the sensor type as source.sensor_name.
	if name := note.Params.Notification.Message.Source["sensor_name"]; name != "" {
		tags["sensor_type"] = name
	} else if sensorType != "" {
		tags["sensor_type"] = sensorType
	}

	ts := time.Now()
	if ms := note.Params.Notification.Timestamp; ms > 0 {
		ts = time.Unix(0, ms*int64(time.Millisecond))
	}

	acc.AddFields("axis_airquality", fields, tags, ts)
}

// dialEventStream performs the digest auth handshake and opens the websocket.
func (a *AxisAirQuality) dialEventStream(ctx context.Context) (*websocket.Conn, error) {
	httpScheme := "http"
	wsScheme := "ws"
	if strings.HasPrefix(a.baseURL, "https://") {
		httpScheme = "https"
		wsScheme = "wss"
	}

	// Step 1: trigger a 401 to obtain the digest challenge.
	challenge, err := a.fetchChallenge(ctx, httpScheme)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	if challenge != nil {
		auth, err := buildAuthHeader(a.user, a.pass, http.MethodGet, eventStreamPath, challenge, 1)
		if err != nil {
			return nil, err
		}
		header.Set("Authorization", auth)
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = time.Duration(a.Timeout)
	if a.InsecureSkipVerify {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	wsURL, err := a.streamURL(wsScheme)
	if err != nil {
		return nil, err
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("websocket dial failed (status %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}
	return conn, nil
}

func (a *AxisAirQuality) fetchChallenge(ctx context.Context, httpScheme string) (*digestChallenge, error) {
	probeURL, err := a.streamURL(httpScheme)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return nil, err
	}
	// Use a bare client (no digest transport) so we receive the raw 401.
	client := &http.Client{
		Timeout: time.Duration(a.Timeout),
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: a.InsecureSkipVerify},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return nil, nil
	}
	challenge := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if challenge == nil {
		return nil, fmt.Errorf("missing or unparseable digest challenge")
	}
	return challenge, nil
}

// allowedFields returns the set of metric field names enabled by the configured
// categories.
func (a *AxisAirQuality) allowedFields() map[string]bool {
	allowed := make(map[string]bool, len(a.Categories))
	for _, c := range a.Categories {
		if field, ok := categoryToField[strings.ToUpper(c)]; ok {
			allowed[field] = true
		}
	}
	return allowed
}

// streamURL builds the event-stream URL from the configured base URL with the
// given scheme (http/https for the digest probe, ws/wss for the websocket).
func (a *AxisAirQuality) streamURL(scheme string) (string, error) {
	u, err := url.Parse(a.baseURL)
	if err != nil {
		return "", err
	}
	u.Scheme = scheme
	u.Path = "/vapix/ws-data-stream"
	u.RawQuery = "sources=events"
	return u.String(), nil
}
