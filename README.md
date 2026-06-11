# telegraf-execd-axis-airquality

A [Telegraf external plugin][external] that collects air quality measurements
from an Axis air quality sensor (for example the [AXIS D6310][d6310]) using the
VAPIX [Air Quality monitor API][api] and feeds them into Telegraf through the
[`inputs.execd`][execd] plugin. From there Telegraf can write to InfluxDB or any
other supported output.

[external]: https://github.com/influxdata/telegraf/blob/master/docs/EXTERNAL_PLUGINS.md
[execd]: https://github.com/influxdata/telegraf/tree/master/plugins/inputs/execd
[d6310]: https://www.axis.com/products/axis-d6310-air-quality-sensor
[api]: https://developer.axis.com/vapix/device-configuration/environmental-sensor/air-quality-api/

> Looking to run the collector **on the camera itself**? See
> [acap_influxdb_data_collector][acap], an ACAP that gathers device metrics and
> air quality data on-device and writes straight to InfluxDB. This repository is
> the off-device alternative that pulls data into Telegraf instead.

[acap]: https://github.com/Mo3he/acap_influxdb_data_collector

## Why this instead of `mqtt_consumer` + `json_v2`?

A common DIY approach is to publish device events to MQTT and parse them in
Telegraf with the generic `mqtt_consumer` input and a `json_v2` parser. That
works, but it puts the burden on you to hand-map every JSON path, and small
mismatches fail in confusing ways:

- A `tag`/`field` path that does not match the payload silently resolves to
  nothing, and a metric with no fields is rejected outright by Telegraf.
- Device identity (serial) usually is not a top-level JSON field; it lives in
  the MQTT topic or a nested `source` object, so it is easy to lose.
- Timestamp formats and units have to be declared correctly or the data lands
  malformed.

This plugin avoids all of that. It talks to the device over VAPIX, emits clean
Telegraf line protocol directly (`data_format = "influx"`), and **auto-discovers
the device serial** as a stable `serial` tag that survives IP changes. There is
no `json_v2` block to write and no paths to guess.

> Scope: this plugin collects **air quality** measurements over VAPIX. It does
> not consume Axis analytics/event streams (e.g. AXIS Object Analytics line
> counting) published over MQTT. For those events, the generic
> `inputs.mqtt_consumer` with a `json_v2` parser is still the right tool.

## Metrics

See the plugin [README](plugins/inputs/axis_airquality/README.md) for the full
list of tags and fields.

## Install

Install the prebuilt binary with Go (requires Go 1.24 or newer):

```sh
go install github.com/Mo3he/telegraf-execd-axis-airquality/cmd/axis_airquality@latest
```

This installs an `axis_airquality` executable into `$(go env GOPATH)/bin`. Make
sure that directory is on your `PATH`, then point your Telegraf `execd` config
at it.

Alternatively, clone the repository and build from source (see [Build](#build)).

## Build

```sh
go build -o axis_airquality ./cmd/axis_airquality
```

This produces a standalone `axis_airquality` binary.

## Configure the plugin

Edit `plugin.conf` (read by the standalone binary, not by Telegraf directly):

```toml
[[inputs.axis_airquality]]
  url = "http://10.0.0.10"
  username = "root"
  password = "secret"
```

`${VAR}` references are expanded from the environment when the config loads, so
keep secrets out of shared files by supplying them via environment variables:

```toml
[[inputs.axis_airquality]]
  url = "http://10.0.0.10"
  username = "${AXIS_USERNAME}"
  password = "${AXIS_PASSWORD}"
```

You can verify the binary on its own. It speaks Telegraf's line protocol on
stdout. Keep stdin open (the shim exits on stdin EOF):

```sh
sleep 30 | ./axis_airquality -config plugin.conf -poll_interval 10s
```

Once it is wired into Telegraf, run Telegraf in test mode to see exactly what
line protocol is produced without writing anything to your output:

```sh
telegraf --config telegraf.conf --test
```

## Run with Telegraf

Add an `execd` input to your `telegraf.conf`, pointing at the built binary and
its config. The plugin has two collection modes (set via `mode` in
`plugin.conf`), and each pairs with a different `execd` setup.

### Stream mode: real-time ~1s data (default, recommended for live monitoring)

The D6310 publishes every measurement once per second over an event websocket.
In `stream` mode (the default) the plugin holds that connection open and pushes
a metric on each update. Run it as a continuous service input with
`signal = "none"` and `-poll_interval_disabled`:

`plugin.conf`:

```toml
[[inputs.axis_airquality]]
  url = "http://10.0.0.10"
  username = "root"
  password = "secret"
  mode = "stream"
```

`telegraf.conf`:

```toml
[[inputs.execd]]
  command = ["/path/to/axis_airquality", "-config", "/path/to/plugin.conf", "-poll_interval_disabled"]
  signal = "none"
  data_format = "influx"

# Example: write to InfluxDB 2.x
[[outputs.influxdb_v2]]
  urls = ["http://10.0.0.20:8086"]
  token = "${INFLUX_TOKEN}"
  organization = "my-org"
  bucket = "airquality"
```

Metrics arrive at the device's native ~1Hz rate; Telegraf's `interval` does not
gate them. On connection loss the plugin reconnects automatically with backoff.

#### Writing to InfluxDB 3

InfluxDB 3 (Core/Enterprise, default port `8181`) has no native v3 output
plugin yet; use the same `influxdb_v2` output against its v2-compatible write
API. A few fields mean different things in v3:

- `bucket` maps to the **database** name, so set it to the database you created.
- `organization` is ignored by v3 and can be any non-empty placeholder.
- `token` must be an InfluxDB 3 token with write permission on that database.

```toml
[[outputs.influxdb_v2]]
  urls = ["http://10.0.0.20:8181"]
  token = "${INFLUX_TOKEN}"
  organization = "placeholder"   # ignored by InfluxDB 3
  bucket = "airquality"          # this is the v3 database name
```

### History mode: polled REST data

In `history` mode the plugin polls the REST `getHistoryData` API. The device
only stores history at 60s granularity, so this mode's effective resolution is
~60s no matter how fast you poll. Use it for simple, low-rate logging or where
a websocket is undesirable. Let Telegraf drive collection via STDIN:

`plugin.conf`:

```toml
[[inputs.axis_airquality]]
  url = "http://10.0.0.10"
  username = "root"
  password = "secret"
  mode = "history"
```

`telegraf.conf`:

```toml
[[inputs.execd]]
  command = ["/path/to/axis_airquality", "-config", "/path/to/plugin.conf", "-poll_interval_disabled"]
  signal = "STDIN"
  interval = "60s"   # 60s matches the history storage granularity
  data_format = "influx"
```

> Alternatively, in history mode the binary can self-schedule with
> `-poll_interval 60s` and `signal = "none"` instead of STDIN.

> InfluxDB 2.x uses token/org/bucket authentication. Create an API token in the
> InfluxDB UI and supply it via the `token` field. Prefer an environment
> variable (`token = "${INFLUX_TOKEN}"`) over an inline secret so credentials
> stay out of shared config files.

## Multiple devices

The execd shim runs a single plugin instance per process, so collect from
multiple Axis sensors by adding one `[[inputs.execd]]` block per device, each
pointing at its own `plugin.conf`:

```toml
[[inputs.execd]]
  command = ["/path/to/axis_airquality", "-config", "/etc/telegraf/d6310-lobby.conf", "-poll_interval_disabled"]
  signal = "none"
  data_format = "influx"

[[inputs.execd]]
  command = ["/path/to/axis_airquality", "-config", "/etc/telegraf/d6310-warehouse.conf", "-poll_interval_disabled"]
  signal = "none"
  data_format = "influx"
```

Each device's metrics are tagged with its `source` (host), `sensor_id`, and a
`serial` (the device serial number, auto-discovered from the device). The
`serial` tag stays stable even if a device's IP changes, so it is the most
reliable way to identify a device in InfluxDB. Give each device a separate
`plugin.conf` with its own `url` and credentials.

## Development

```sh
make build   # build the binary
make test    # run unit tests
make vet     # run go vet
make fmt     # format the code
```

## License

[MIT](LICENSE)
