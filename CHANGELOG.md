# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-06-10

Initial release of the `axis_airquality` Telegraf external `execd` input plugin.

### Added

- Stream mode (default): subscribes to the device event websocket for
  real-time measurements at the native ~1 Hz update rate.
- History mode: polls the VAPIX Air Quality monitor REST API (effective ~60s
  resolution, matching the device's history granularity).
- HTTP Digest authentication (MD5, `qop="auth"`).
- Configurable measurement categories: `TEMPERATURE`, `HUMIDITY`, `CO2`,
  `NOX`, `PM1_0`, `PM2_5`, `PM4_0`, `PM10_0`, `VOC`, `AQI`, plus the optional
  derived comfort indices `HEAT_INDEX` and `HUMIDEX`.
- Automatic default-sensor discovery when `sensor_id` is not set.
- `connected` status field and `source` / `sensor_id` / `sensor_type` tags.

### Tested with

- **Device:** AXIS D6310 Air Quality Sensor
- **AXIS OS (firmware):** 12.9.57
- **Telegraf:** v1.39.0
- **Go:** 1.26.x (module targets Go 1.24)

[0.1.0]: https://github.com/Mo3he/telegraf-execd-axis-airquality/releases/tag/v0.1.0
