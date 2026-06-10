package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/influxdata/telegraf/plugins/common/shim"

	// Register the axis_airquality input plugin.
	_ "github.com/Mo3he/telegraf-execd-axis-airquality/plugins/inputs/axis_airquality"
)

var (
	pollInterval         = flag.Duration("poll_interval", 1*time.Minute, "how often to send metrics")
	pollIntervalDisabled = flag.Bool("poll_interval_disabled", false, "disable polling and let the plugin push metrics on its own schedule (use for stream mode)")
	configFile           = flag.String("config", "", "path to the plugin config file")
)

func main() {
	flag.Parse()

	if *pollIntervalDisabled {
		*pollInterval = shim.PollIntervalDisabled
	}

	s := shim.New()

	if err := s.LoadConfig(configFile); err != nil {
		fmt.Fprintf(os.Stderr, "loading config failed: %s\n", err)
		os.Exit(1)
	}

	if err := s.Run(*pollInterval); err != nil {
		fmt.Fprintf(os.Stderr, "running plugin failed: %s\n", err)
		os.Exit(1)
	}
}
