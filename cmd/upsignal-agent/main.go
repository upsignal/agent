// Command upsignal-agent collects Linux host metrics for Upsignal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/upsignal/agent/internal/agent"
	"github.com/upsignal/agent/internal/collector"
	"github.com/upsignal/agent/internal/config"
	"github.com/upsignal/agent/internal/logging"
	"github.com/upsignal/agent/internal/telemetry"
	"github.com/upsignal/agent/internal/version"
)

const usage = `Usage: upsignal-agent <command> [--config path]

Commands:
  run        Run the agent (used by the systemd service)
  status     Show version, configuration and enrollment state
  diagnose   Check configuration and take a test sample
  enroll     Enroll this server with Upsignal
  version    Print version information
`

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.DefaultPath, "path to config.yaml")
	if err := flags.Parse(rest); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}

	switch cmd {
	case "run":
		return runAgent(*configPath, stderr)
	case "status":
		return status(*configPath, stdout)
	case "diagnose":
		return diagnose(*configPath, stdout)
	case "enroll":
		fmt.Fprintln(stderr, "enroll is not available yet: it needs the backend agent API (milestone M3)")
		return exitError
	case "version":
		fmt.Fprintln(stdout, version.String())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
	return exitUsage
}

func runAgent(configPath string, stderr io.Writer) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	log := logging.New(stderr, cfg.Logging.Level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := agent.New(cfg, log).Run(ctx); err != nil {
		log.Error("agent failed", "error", err)
		return exitError
	}
	return exitOK
}

func status(configPath string, stdout io.Writer) int {
	fmt.Fprintln(stdout, version.String())
	code := exitOK
	if _, err := config.Load(configPath); err != nil {
		fmt.Fprintf(stdout, "config:      %s (%v)\n", configPath, err)
		code = exitError
	} else {
		fmt.Fprintf(stdout, "config:      %s (valid)\n", configPath)
	}
	fmt.Fprintf(stdout, "enrollment:  %s\n", enrollmentState())
	fmt.Fprintln(stdout, "last upload: not available yet (telemetry delivery is milestone M4)")
	fmt.Fprintln(stdout, "service:     see `systemctl status upsignal-agent`")
	return code
}

// enrollmentState only checks that the credential file exists; it never reads or prints the credential.
func enrollmentState() string {
	_, err := os.Stat(filepath.Join(config.DefaultStateDir, "agent-credential"))
	switch {
	case err == nil:
		return "enrolled"
	case errors.Is(err, fs.ErrNotExist):
		return "not enrolled"
	case errors.Is(err, fs.ErrPermission):
		return "unknown (permission denied; run with sudo)"
	}
	return fmt.Sprintf("unknown (%v)", err)
}

func diagnose(configPath string, stdout io.Writer) int {
	code := exitOK
	fmt.Fprintln(stdout, version.String())
	if _, err := config.Load(configPath); err != nil {
		fmt.Fprintf(stdout, "\n[FAIL] config %s: %v\n", configPath, err)
		code = exitError
	} else {
		fmt.Fprintf(stdout, "\n[ OK ] config %s\n", configPath)
	}

	c := collector.New()
	host, hostErrs := c.CollectHost()
	// CPU utilization needs two readings; the first one only primes the counters.
	c.Collect()
	time.Sleep(time.Second)
	sample, errs := c.Collect()
	errs = append(hostErrs, errs...)

	if len(errs) == 0 {
		fmt.Fprintln(stdout, "[ OK ] metric collection")
	} else {
		code = exitError
		for _, e := range errs {
			fmt.Fprintf(stdout, "[FAIL] collector %s: %s\n", e.Collector, e.Message)
		}
	}
	fmt.Fprintln(stdout, "[SKIP] DNS, TLS and API reachability (needs milestone M3)")

	out, _ := json.MarshalIndent(struct {
		Host   telemetry.Host   `json:"host"`
		Sample telemetry.Sample `json:"sample"`
	}{host, sample}, "", "  ")
	fmt.Fprintf(stdout, "\nTest sample:\n%s\n", out)
	return code
}
