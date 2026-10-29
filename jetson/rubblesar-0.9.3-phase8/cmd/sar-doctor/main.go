// Command sar-doctor checks that this machine is ready for RubbleSAR. It is a
// pure Go binary (no cgo) so it runs before zenoh-c or anything else exists.
//
//	sar-doctor [--role auto|jetson|zbook] [--endpoint HOST:PORT] [--config FILE]
//
// Exit codes: 0 no failures (warnings allowed), 1 at least one failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/doctor"
	"rubblesar/internal/version"
)

const tool = "sar-doctor"

func main() {
	role := flag.String("role", "auto", "auto, jetson or zbook")
	endpoint := flag.String("endpoint", "", "zenoh router to test, HOST:PORT (default from config)")
	cfgPath := flag.String("config", "", "config file to read the zenoh endpoint from")
	rest := flag.String("rest", doctor.DefaultRestURL, "zenohd REST admin URL (ZBook)")
	stage := flag.String("stage", "", "'prereqs' = report items installed by later setup steps as TODO")
	reportDir := flag.String("report-dir", "reports", "where to save the JSON report")
	ver := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *ver {
		fmt.Println(version.String(tool))
		return
	}
	sys := doctor.RealSys{}
	if *role == "auto" {
		*role = doctor.DetectRole(sys)
	}
	if *role != "jetson" && *role != "zbook" {
		fmt.Fprintln(os.Stderr, "--role must be auto, jetson or zbook")
		os.Exit(cli.ExitSetup)
	}
	if *endpoint == "" {
		*endpoint = endpointFromConfig(*role, *cfgPath)
	}
	fmt.Printf("%s\nChecking this machine as the %s\n", version.String(tool), strings.ToUpper(*role))
	if *stage != "" && *stage != "prereqs" {
		fmt.Fprintln(os.Stderr, "--stage must be empty or prereqs")
		os.Exit(cli.ExitSetup)
	}
	if *stage == "prereqs" {
		fmt.Println("Stage: after 00-prereqs. Items installed by later steps are shown as TODO.")
	}
	r := doctor.Run(context.Background(), sys, doctor.Options{Role: *role, Endpoint: *endpoint, RestURL: *rest, Stage: *stage}, os.Stdout)
	if _, err := r.Finish(*reportDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if !r.Passed() {
		os.Exit(cli.ExitFail)
	}
}

// endpointFromConfig finds the first zenoh endpoint as HOST:PORT.
func endpointFromConfig(role, path string) string {
	cli.LoadEnvFile("/etc/rubblesar/edge-eye.env")
	var eps []string
	if role == "jetson" {
		if path == "" {
			path = cli.DefaultConfig("/etc/rubblesar/edge-eye.toml", "configs/jetson/edge-eye.toml")
		}
		if c, err := config.LoadEdge(path); err == nil {
			eps = c.Zenoh.Connect
		} else if v := os.Getenv("SAR_ZENOH_CONNECT"); v != "" {
			eps = strings.Split(v, ",")
		}
	} else {
		if path == "" {
			path = cli.DefaultConfig("/etc/rubblesar/sar.toml", "configs/zbook/sar.toml")
		}
		if c, err := config.LoadZBook(path); err == nil {
			eps = c.Zenoh.Connect
		}
	}
	if len(eps) == 0 {
		if role == "zbook" {
			return "127.0.0.1:7447"
		}
		return ""
	}
	_, hp, _ := strings.Cut(strings.TrimSpace(eps[0]), "/")
	return hp
}
