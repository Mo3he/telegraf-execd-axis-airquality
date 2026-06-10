# Axis Air Quality Input Plugin

Collects air quality measurements from an Axis air quality sensor (for example
the [AXIS D6310](https://www.axis.com/products/axis-d6310-air-quality-sensor))
through the VAPIX [Air Quality monitor API][api].

The plugin supports two collection modes:

- **`stream`** (default): subscribes to the live
  `tnsaxis:AirQualityMonitor/Metadata` event over a websocket and emits a
  metric on every update, at the device's native ~1s rate. Run it as a service
  input (`signal = "none"`, `-poll_interval 0`).
- **`history`**: polls the REST `getHistoryData` action for each configured
  category and reports the most recent measurement. The device only stores
  history at 60s granularity, so this mode's effective resolution is ~60s
  regardless of how often you poll.

[api]: https://developer.axis.com/vapix/device-configuration/environmental-sensor/air-quality-api/

## Requirements

- An Axis device that supports the Air Quality monitor API.
- A device account (digest authentication is used automatically).

## Configuration

```toml @sample.conf
# Read air quality measurements from an Axis air quality sensor (e.g. AXIS D6310)
# via the VAPIX Air Quality monitor API.
[[inputs.axis_airquality]]
  ## Base URL of the Axis device. Use https:// if the device requires it.
  url = "http://10.0.0.10"

  ## Device credentials. Digest authentication is used automatically.
  username = "root"
  password = "secret"

  ## Collection mode (default "stream"):
  ##   "stream"  - subscribe to the live event websocket for real-time data
  ##               at the device's native ~1s update rate. Run this as a
  ##               service input: signal = "none" and -poll_interval 0.
  ##   "history" - poll the REST history API (max ~60s effective resolution,
  ##               since the device stores history at 60s granularity).
  # mode = "stream"

  ## Sensor ID to query. When empty, the default sensor reported by the
  ## device is used.
  # sensor_id = ""

  ## Categories to collect. Defaults to all numeric measurements.
  ## Supported: TEMPERATURE, HUMIDITY, CO2, NOX, PM1_0, PM2_5, PM4_0,
  ##            PM10_0, VOC, AQI, HEAT_INDEX, HUMIDEX
  # categories = ["TEMPERATURE", "HUMIDITY", "CO2", "NOX", "PM1_0", "PM2_5", "PM4_0", "PM10_0", "VOC", "AQI"]

  ## How far back to query history when fetching the latest measurement.
  ## The most recent sample within this window is reported. (history mode only)
  # lookback = "5m"

  ## HTTP request timeout.
  # timeout = "10s"

  ## Skip TLS certificate verification (useful for self-signed device certs).
  # insecure_skip_verify = false
```

## Metrics

- `axis_airquality`
  - tags:
    - `source` (device host)
    - `sensor_id`
    - `sensor_type` (e.g. `D6310`)
  - fields:
    - `temperature` (float, °C or °F per device scale)
    - `humidity` (float, %)
    - `co2` (float, ppm)
    - `nox` (float, index)
    - `pm1_0` (float, µg/m³)
    - `pm2_5` (float, µg/m³)
    - `pm4_0` (float, µg/m³)
    - `pm10_0` (float, µg/m³)
    - `voc` (float, index)
    - `aqi` (float)
    - `heat_index` (float, optional)
    - `humidex` (float, optional)
    - `connected` (bool)

Only configured categories that return data are emitted as fields.

## Example Output

```text
axis_airquality,sensor_id=0,sensor_type=D6310,source=10.0.0.10 temperature=20.5,humidity=33.7,co2=429,nox=1,pm1_0=0,pm2_5=0,pm4_0=0,pm10_0=0,voc=476,aqi=2,connected=true 1781076359000000000
```
