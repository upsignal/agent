// Command upsignal-agent collects Linux host metrics for Upsignal.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/upsignal/agent/internal/agent"
	"github.com/upsignal/agent/internal/collector"
	"github.com/upsignal/agent/internal/config"
	"github.com/upsignal/agent/internal/logging"
	"github.com/upsignal/agent/internal/state"
	"github.com/upsignal/agent/internal/telemetry"
	"github.com/upsignal/agent/internal/transport"
	"github.com/upsignal/agent/internal/version"
)

const usage = `Usage: upsignal-agent <command> [flags]

Commands:
  run        Run the agent (used by the systemd service)
  enroll     Enroll this server with the one-time token from Upsignal
  status     Show version, enrollment and upload state
  diagnose   Check configuration, metric collection and API connectivity
  version    Print version information

Flags:
  --config path   config file (default /etc/upsignal-agent/config.yaml)
  --force         enroll: replace an existing enrollment
`

// Exit codes. exitRevoked belongs in the systemd unit's RestartPreventExitStatus, so a revoked
// agent stays stopped instead of restarting in a loop.
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitRevoked = 3
)

// The package creates this system user; files are handed to it when enroll runs as root.
const serviceUser = "upsignal-agent"

// stateDir is a variable so tests can point it at a temporary directory.
var stateDir = config.DefaultStateDir

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.DefaultPath, "path to config.yaml")
	force := flags.Bool("force", false, "replace an existing enrollment")
	if err := flags.Parse(rest); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		// Never accept the token as an argument: it would end up in shell history and ps output.
		fmt.Fprintf(stderr, "unexpected argument %q (the enrollment token is read from a prompt or stdin)\n", flags.Arg(0))
		return exitUsage
	}

	switch cmd {
	case "run":
		return runAgent(*configPath, stderr)
	case "enroll":
		return enroll(*configPath, *force, stdin, stdout, stderr)
	case "status":
		return status(*configPath, stdout)
	case "diagnose":
		return diagnose(*configPath, stdout)
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
	a, err := agent.New(cfg, log, stateDir)
	if err != nil {
		log.Error("cannot start", "error", err)
		return exitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	switch err := a.Run(ctx); {
	case errors.Is(err, agent.ErrRevoked):
		log.Error("stopping", "error", err)
		return exitRevoked
	case err != nil:
		log.Error("agent failed", "error", err)
		return exitError
	}
	return exitOK
}

func enroll(configPath string, force bool, stdin io.Reader, stdout, stderr io.Writer) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if existing, err := state.LoadCredential(stateDir); err == nil && !force {
		fmt.Fprintf(stderr, "already enrolled as %s; pass --force to replace it\n", existing)
		return exitError
	} else if err != nil && !errors.Is(err, state.ErrNotEnrolled) && !force {
		fmt.Fprintf(stderr, "cannot read the current enrollment: %v\n", err)
		return exitError
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "cannot create %s: %v (run with sudo)\n", stateDir, err)
		return exitError
	}
	token, err := readToken(stdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	client, err := transport.New(cfg.API.BaseURL, time.Duration(cfg.API.RequestTimeout))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	host, _ := collector.New().CollectHost()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.API.RequestTimeout))
	defer cancel()
	res, err := client.Enroll(ctx, transport.EnrollRequest{
		EnrollmentToken: token, AgentVersion: version.Version, Host: &host,
	})
	if err != nil {
		var apiErr *transport.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "INVALID_ENROLLMENT_TOKEN" {
			fmt.Fprintln(stderr, "enrollment failed: the token is invalid, already used, or expired. Issue a new one in Upsignal.")
		} else {
			fmt.Fprintf(stderr, "enrollment failed: %v\n", err)
		}
		return exitError
	}
	owner := serviceOwner()
	cred := state.Credential{
		AgentID: res.AgentID, MonitorID: res.MonitorID, Secret: res.Credential,
		BaseURL: cfg.API.BaseURL, EnrolledAt: time.Now().UTC(),
	}
	if err := state.SaveCredential(stateDir, cred, owner); err != nil {
		fmt.Fprintf(stderr, "enrolled, but could not save the credential: %v\n", err)
		return exitError
	}
	if owner.UID >= 0 {
		if err := chownTree(stateDir, owner); err != nil {
			fmt.Fprintf(stderr, "warning: could not hand %s to %s: %v\n", stateDir, serviceUser, err)
		}
	}
	fmt.Fprintf(stdout, "Enrolled as %s.\nStart reporting with: sudo systemctl enable --now upsignal-agent\n", cred)
	return exitOK
}

// readToken prompts without echo on a terminal, or reads one line from stdin for automation:
//
//	echo "$TOKEN" | sudo upsignal-agent enroll
func readToken(stdin io.Reader, stderr io.Writer) (string, error) {
	var raw string
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Enrollment token: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		raw = string(b)
	} else {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read token: %w", err)
		}
		raw = line
	}
	token := strings.TrimSpace(raw)
	if b, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(b) != 32 {
		return "", errors.New("that does not look like an enrollment token (43 characters from Upsignal)")
	}
	return token, nil
}

// serviceOwner returns the service user's ids when enroll runs as root and the user exists.
func serviceOwner() state.Owner {
	if os.Geteuid() != 0 {
		return state.KeepOwner
	}
	u, err := user.Lookup(serviceUser)
	if err != nil {
		return state.KeepOwner
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return state.KeepOwner
	}
	return state.Owner{UID: uid, GID: gid}
}

// chownTree hands the state directory and everything in it (including spool files) to owner.
func chownTree(dir string, owner state.Owner) error {
	return filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, owner.UID, owner.GID)
	})
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
	cred, err := state.LoadCredential(stateDir)
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "enrollment:  %s, since %s\n", cred, cred.EnrolledAt.Format(time.RFC3339))
	case errors.Is(err, state.ErrNotEnrolled):
		fmt.Fprintln(stdout, "enrollment:  not enrolled (run `sudo upsignal-agent enroll`)")
		code = exitError
	case errors.Is(err, os.ErrPermission):
		fmt.Fprintln(stdout, "enrollment:  unknown (permission denied; run with sudo)")
	default:
		fmt.Fprintf(stdout, "enrollment:  %v\n", err)
		code = exitError
	}
	st, err := state.ReadStatus(stateDir)
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "last upload: %s\n", formatTime(st.LastUploadAt))
		if st.LastErrorCategory != "" {
			fmt.Fprintf(stdout, "last error:  %s at %s\n", st.LastErrorCategory, formatTime(st.LastErrorAt))
		}
		fmt.Fprintf(stdout, "spooled:     %d waiting\n", st.SpooledFiles)
		if st.Revoked {
			fmt.Fprintln(stdout, "revoked:     yes; enroll again with --force")
			code = exitError
		}
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintln(stdout, "last upload: never")
	case errors.Is(err, os.ErrPermission):
		fmt.Fprintln(stdout, "last upload: unknown (permission denied; run with sudo)")
	}
	fmt.Fprintln(stdout, "service:     see `systemctl status upsignal-agent`")
	return code
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return fmt.Sprintf("%s (%s ago)", t.Format(time.RFC3339), time.Since(*t).Round(time.Second))
}

func diagnose(configPath string, stdout io.Writer) int {
	code := exitOK
	fail := func(format string, args ...any) {
		code = exitError
		fmt.Fprintf(stdout, "[FAIL] "+format+"\n", args...)
	}
	ok := func(format string, args ...any) { fmt.Fprintf(stdout, "[ OK ] "+format+"\n", args...) }
	fmt.Fprintln(stdout, version.String())
	fmt.Fprintln(stdout)

	cfg, cfgErr := config.Load(configPath)
	if cfgErr != nil {
		fail("config %s: %v", configPath, cfgErr)
	} else {
		ok("config %s", configPath)
	}

	switch cred, err := state.LoadCredential(stateDir); {
	case err == nil:
		ok("enrolled as %s", cred)
	case errors.Is(err, os.ErrPermission):
		fmt.Fprintln(stdout, "[SKIP] enrollment (permission denied; run with sudo)")
	default:
		fail("enrollment: %v", err)
	}

	c := collector.New()
	host, hostErrs := c.CollectHost()
	// CPU utilization needs two readings; the first one only primes the counters.
	c.Collect()
	time.Sleep(time.Second)
	sample, errs := c.Collect()
	errs = append(hostErrs, errs...)
	if len(errs) == 0 {
		ok("metric collection")
	}
	for _, e := range errs {
		fail("collector %s: %s", e.Collector, e.Message)
	}

	if cfgErr == nil {
		checkConnectivity(cfg, ok, fail)
	}

	out, _ := json.MarshalIndent(struct {
		Host   telemetry.Host   `json:"host"`
		Sample telemetry.Sample `json:"sample"`
	}{host, sample}, "", "  ")
	fmt.Fprintf(stdout, "\nTest sample:\n%s\n", out)
	return code
}

// checkConnectivity tests DNS, a verified TLS handshake, and GET /v1/healthz. It sends no credential.
func checkConnectivity(cfg config.Config, ok, fail func(string, ...any)) {
	u, _ := url.Parse(cfg.API.BaseURL)
	timeout := time.Duration(cfg.API.RequestTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), 3*timeout)
	defer cancel()
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		fail("DNS %s: %v", host, err)
		return
	}
	ok("DNS %s (%d addresses)", host, len(addrs))
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		fail("TLS %s: %v", net.JoinHostPort(host, port), err)
		return
	}
	tlsVersion := conn.(*tls.Conn).ConnectionState().Version
	conn.Close()
	ok("TLS %s (%s, certificate verified)", net.JoinHostPort(host, port), tls.VersionName(tlsVersion))
	client, err := transport.New(cfg.API.BaseURL, timeout)
	if err != nil {
		fail("API client: %v", err)
		return
	}
	if err := client.Health(ctx); err != nil {
		fail("API health %s/v1/healthz: %v", cfg.API.BaseURL, err)
		return
	}
	ok("API reachable")
}
