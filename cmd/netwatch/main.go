// netwatch monitors internet connectivity from an always-on host (a Raspberry
// Pi, typically) and records outages to a log file.
//
// While healthy it probes a single primary target. Only when that probe fails
// does it also probe a secondary target, so the steady-state cost is one ICMP
// echo per interval. An outage is declared once both targets have failed in the
// same round, -failures times consecutively, and is closed out when
// connectivity returns.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type config struct {
	primary   string
	secondary string
	interval  time.Duration
	timeout   time.Duration
	failures  int
	logPath   string
	recover   string
	quiet     bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.primary, "primary", "1.1.1.1", "primary target to probe (IPv4 address or hostname)")
	flag.StringVar(&cfg.secondary, "secondary", "8.8.8.8", "secondary target, probed only when the primary fails")
	flag.DurationVar(&cfg.interval, "interval", 5*time.Second, "time between probe rounds")
	flag.DurationVar(&cfg.timeout, "timeout", 2*time.Second, "per-probe reply timeout")
	flag.IntVar(&cfg.failures, "failures", 3, "consecutive rounds with both targets down before declaring an outage")
	flag.StringVar(&cfg.logPath, "log", "/var/log/netwatch.log", "outage log file (appended to; reopened on SIGHUP)")
	flag.StringVar(&cfg.recover, "recover", "both", "what ends an outage: \"both\" targets responding, or \"any\" one of them")
	flag.BoolVar(&cfg.quiet, "quiet", false, "log only to the file, not to stdout")
	flag.Parse()

	if cfg.recover != "both" && cfg.recover != "any" {
		fatalf("-recover must be %q or %q, got %q", "both", "any", cfg.recover)
	}
	if cfg.failures < 1 {
		fatalf("-failures must be at least 1, got %d", cfg.failures)
	}
	if cfg.interval <= 0 {
		fatalf("-interval must be positive, got %s", cfg.interval)
	}

	lw, err := newLogWriter(cfg.logPath, !cfg.quiet)
	if err != nil {
		fatalf("opening log: %v", err)
	}
	defer lw.Close()
	logger := log.New(lw, "", 0)

	p, err := newPinger()
	if err != nil {
		logger.Print(logLine("ERROR", err.Error()))
		fatalf("%v", err)
	}
	defer p.Close()

	// Reopen the log on SIGHUP so logrotate doesn't leave us writing into a
	// deleted inode.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := lw.Reopen(); err != nil {
				fmt.Fprintf(os.Stderr, "netwatch: reopening log: %v\n", err)
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	mode := "raw socket"
	if p.udp {
		mode = "unprivileged datagram socket"
	}
	logger.Print(logLine("START", fmt.Sprintf(
		"primary=%s secondary=%s interval=%s timeout=%s failures=%d recover=%s (%s)",
		cfg.primary, cfg.secondary, cfg.interval, cfg.timeout, cfg.failures, cfg.recover, mode)))

	done := make(chan struct{})
	go func() {
		<-stop
		close(done)
	}()

	run(cfg, func(target string) error {
		dst, err := resolve(target, cfg.timeout)
		if err != nil {
			return err
		}
		_, err = p.Ping(dst, cfg.timeout)
		return err
	}, logger, done)
	logger.Print(logLine("STOP", "shutting down"))
}

// prober reports whether a target answered. A nil error means it is up.
type prober func(target string) error

// run is the monitor loop. It returns when a signal arrives on stop.
func run(cfg config, probe prober, logger *log.Logger, stop <-chan struct{}) {
	var (
		consecutive   int  // rounds in which both targets were down
		inOutage      bool // true between OUTAGE_START and OUTAGE_END
		outageStart   time.Time
		lastPrimary   error // most recent failure reasons, for the log line
		lastSecondary error
	)

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	for {
		primaryErr := probe(cfg.primary)

		// The secondary is only worth the packet when the primary looks down,
		// or when we're in an outage and waiting on both to return.
		var secondaryErr error
		checkedSecondary := false
		if primaryErr != nil || (inOutage && cfg.recover == "both") {
			secondaryErr = probe(cfg.secondary)
			checkedSecondary = true
		}

		bothDown := primaryErr != nil && checkedSecondary && secondaryErr != nil
		if primaryErr != nil {
			lastPrimary = primaryErr
		}
		if checkedSecondary && secondaryErr != nil {
			lastSecondary = secondaryErr
		}

		if bothDown {
			consecutive++
			if !inOutage && consecutive >= cfg.failures {
				inOutage = true
				outageStart = time.Now()
				logger.Print(logLine("OUTAGE_START", fmt.Sprintf(
					"both targets unreachable for %d consecutive rounds; %s: %s; %s: %s",
					consecutive, cfg.primary, reason(lastPrimary), cfg.secondary, reason(lastSecondary))))
			}
		} else {
			// In "both" mode an open outage needs both targets answering in the
			// same round. Everywhere else a single success is enough, either to
			// close the outage or just to reset the failure counter.
			recovered := primaryErr == nil || (checkedSecondary && secondaryErr == nil)
			if inOutage && cfg.recover == "both" {
				recovered = primaryErr == nil && checkedSecondary && secondaryErr == nil
			}
			if recovered {
				if inOutage {
					d := time.Since(outageStart).Round(time.Second)
					logger.Print(logLine("OUTAGE_END", fmt.Sprintf(
						"connectivity restored after %s (started %s)",
						d, outageStart.Format(time.RFC3339))))
					inOutage = false
				}
				consecutive = 0
			}
		}

		select {
		case <-stop:
			if inOutage {
				d := time.Since(outageStart).Round(time.Second)
				logger.Print(logLine("OUTAGE_OPEN", fmt.Sprintf(
					"exiting while still in an outage; open for %s (started %s)",
					d, outageStart.Format(time.RFC3339))))
			}
			return
		case <-ticker.C:
		}
	}
}

func reason(err error) string {
	if err == nil {
		return "ok"
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "timeout"
	}
	return err.Error()
}

func logLine(event, detail string) string {
	return fmt.Sprintf("%s %s %s", time.Now().Format(time.RFC3339), event, detail)
}

// logWriter appends to the outage log and optionally mirrors to stdout so that
// journald picks the lines up when running under systemd.
type logWriter struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	stdout bool
}

func newLogWriter(path string, stdout bool) (*logWriter, error) {
	lw := &logWriter{path: path, stdout: stdout}
	if err := lw.Reopen(); err != nil {
		return nil, err
	}
	return lw, nil
}

func (l *logWriter) Reopen() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.mu.Lock()
	old := l.f
	l.f = f
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

func (l *logWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stdout {
		os.Stdout.Write(b)
	}
	n, err := l.f.Write(b)
	if err != nil {
		return n, err
	}
	// Outages tend to end in a power cut or a reboot; don't lose the line to
	// the page cache.
	return n, l.f.Sync()
}

func (l *logWriter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

var _ io.Writer = (*logWriter)(nil)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "netwatch: "+format+"\n", args...)
	os.Exit(1)
}
