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

You can verify the binary on its own. It speaks Telegraf's line protocol on
stdout. Keep stdin open (the shim exits on stdin EOF):

```sh
sleep 30 | ./axis_airquality -config plugin.conf -poll_interval 10s
```

## Run with Telegraf

Add an `execd` input to your `telegraf.conf`, pointing at the built binary and
its config. The plugin has two collection modes (set via `mode` in
`plugin.conf`), and each pairs with a different `execd` setup.

### Stream mode: real-time ~1s data (default, recommended for live monitoring)

The D6310 publishes every measurement once per second over an event websocket.
In `stream` mode (the default) the plugin holds that connection open and pushes
a metric on each update. Run it as a continuous service input with
`signal = "none"` and `-poll_interval 0`:

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
  command = ["/path/to/axis_airquality", "-config", "/path/to/plugin.conf", "-poll_interval", "0"]
  signal = "none"
  data_format = "influx"

# Example: write to InfluxDB 2.x
[[outputs.influxdb_v2]]
  urls = ["http://10.0.0.20:8086"]
  token = "$INFLUX_TOKEN"
  organization = "my-org"
  bucket = "airquality"
```

Metrics arrive at the device's native ~1Hz rate; Telegraf's `interval` does not
gate them. On connection loss the plugin reconnects automatically with backoff.

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
  command = ["/path/to/axis_airquality", "-config", "/path/to/plugin.conf", "-poll_interval", "0"]
  signal = "STDIN"
  interval = "60s"   # 60s matches the history storage granularity
  data_format = "influx"
```

> Alternatively, in history mode the binary can self-schedule with
> `-poll_interval 60s` and `signal = "none"` instead of STDIN.

> InfluxDB 2.x uses token/org/bucket authentication. Create an API token in the
> InfluxDB UI and supply it via the `token` field (or an environment variable).

## Multiple devices

The execd shim runs a single plugin instance per process, so collect from
multiple Axis sensors by adding one `[[inputs.execd]]` block per device, each
pointing at its own `plugin.conf`:

```toml
[[inputs.execd]]
  command = ["/path/to/axis_airquality", "-config", "/etc/telegraf/d6310-lobby.conf", "-poll_interval", "0"]
  signal = "none"
  data_format = "influx"

[[inputs.execd]]
  command = ["/path/to/axis_airquality", "-config", "/etc/telegraf/d6310-warehouse.conf", "-poll_interval", "0"]
  signal = "none"
  data_format = "influx"
```

Each device's metrics are tagged with its `source` (host) and `sensor_id`, so
they stay distinguishable in InfluxDB. Give each device a separate `plugin.conf`
with its own `url` and credentials.

## Development

```sh
make build   # build the binary
make test    # run unit tests
make vet     # run go vet
make fmt     # format the code
```

## License

[MIT](LICENSE)
