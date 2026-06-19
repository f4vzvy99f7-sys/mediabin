package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MaxDillon/daemonizer/daemon"
	"golang.org/x/crypto/ssh/terminal"
)

// stringSlice is a flag.Value that accumulates multiple -t/--tag values.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func requireDaemon(clientErr error, cmd string) {
	if clientErr != nil {
		fmt.Fprintf(os.Stderr, "daemon is not running — use '%s start' to start it\n", os.Args[0])
		os.Exit(1)
	}
}

func main() {
	if os.Getenv("__DAEMON_SERVICE") != "" {
		InitMediabin()
		return
	}

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <start|stop|i|ps|ls|tags|du|logs> [args...]\n", os.Args[0])
		os.Exit(1)
	}

	cmd := os.Args[1]

	client, clientErr := InitMediabin()

	if clientErr != nil && !errors.Is(clientErr, daemon.ErrNotRunning) {
		fmt.Fprintf(os.Stderr, "error: %v\n", clientErr)
		os.Exit(1)
	}

	switch cmd {
	case "start":
		if clientErr == nil {
			fmt.Println("daemon already running")
			return
		}

		startFlags := flag.NewFlagSet("start", flag.ExitOnError)
		dataDirFlag := startFlags.String("data-dir", "./data", "directory where downloaded media is stored")
		portFlag := startFlags.String("port", "8080", "port for the HTTP API server")
		startFlags.Parse(os.Args[2:])

		dataDir, err := filepath.Abs(*dataDirFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: could not resolve data-dir: %v\n", err)
			os.Exit(1)
		}

		fmt.Print("Enter password: ")
		password, err := terminal.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			var passwordStr string
			fmt.Scanln(&passwordStr)
			password = []byte(passwordStr)
		}
		fmt.Println()

		env := map[string]string{
			"DB_PASSWD":  string(password),
			"DB_DATADIR": dataDir,
			"DB_PORT":    *portFlag,
		}
		if err := daemon.Start(client, env); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("daemon started")
		return

	case "stop":
		if clientErr != nil {
			fmt.Fprintf(os.Stderr, "daemon is not running\n")
			os.Exit(1)
		}
		if err := daemon.Stop(client); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("daemon stopped")
		return

	case "i", "install":
		requireDaemon(clientErr, cmd)
		url := ""
		if len(os.Args) > 2 {
			url = os.Args[2]
		}
		if err := client.RegisterNewDownload(url); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

	case "ls":
		requireDaemon(clientErr, cmd)

		lsFlags := flag.NewFlagSet("ls", flag.ExitOnError)
		idsOnly := lsFlags.Bool("ids", false, "print IDs only")
		query := lsFlags.String("q", "", "filter by title (case-insensitive, partial match)")
		lsFlags.StringVar(query, "query", "", "filter by title (case-insensitive, partial match)")
		var tags stringSlice
		lsFlags.Var(&tags, "t", "filter by tag (can be specified multiple times)")
		lsFlags.Var(&tags, "tag", "filter by tag (can be specified multiple times)")
		lsFlags.Parse(os.Args[2:])

		resp, err := client.ListMedia(*query, []string(tags))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		tty := isTTY()
		for _, m := range resp.Media {
			if *idsOnly {
				fmt.Println(m.ID)
				continue
			}
			shortID := m.ID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			if tty {
				fmt.Printf("%s%s%s -- %s%s\n", colorYellow, shortID, colorGray, colorReset, m.Title)
			} else {
				fmt.Printf("%s -- %s\n", shortID, m.Title)
			}
		}

	case "tags":
		requireDaemon(clientErr, cmd)
		tags, err := client.ListTags()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		tty := isTTY()
		for _, tag := range tags {
			if tty {
				parts := strings.SplitN(tag, ":", 2)
				if len(parts) == 2 {
					fmt.Printf("%s%s:%s%s\n", colorGray, parts[0], colorReset, parts[1])
				} else {
					fmt.Println(tag)
				}
			} else {
				fmt.Println(tag)
			}
		}

	case "ps":
		requireDaemon(clientErr, cmd)
		resp, err := client.ListCurrentProcs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if len(resp.Processes) == 0 {
			fmt.Println("no active downloads")
			return
		}
		tty := isTTY()
		for _, p := range resp.Processes {
			if p.IsPending {
				if tty {
					fmt.Printf("[%spending%s] %s\n", colorGray, colorReset, p.Title)
				} else {
					fmt.Printf("[pending] %s\n", p.Title)
				}
				continue
			}
			if tty {
				var color string
				switch {
				case p.Percent < 30:
					color = colorRed
				case p.Percent < 60:
					color = colorYellow
				default:
					color = colorGreen
				}
				fmt.Printf("[%s%6.2f%%%s] %s\n", color, p.Percent, colorReset, p.Title)
			} else {
				fmt.Printf("[%6.2f%%] %s\n", p.Percent, p.Title)
			}
		}

	case "du":
		requireDaemon(clientErr, cmd)
		resp, err := client.DiskUsage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Disk space for media directory: %s\n", resp.Path)
		fmt.Printf("  Total: %s\n", formatBytes(resp.TotalBytes))
		fmt.Printf("  Used:  %s\n", formatBytes(resp.UsedBytes))
		fmt.Printf("  Free:  %s\n", formatBytes(resp.FreeBytes))

	case "logs":
		requireDaemon(clientErr, cmd)

		logsFlags := flag.NewFlagSet("logs", flag.ExitOnError)
		archiveFlag := logsFlags.Bool("new", false, "archive the current log file and start a fresh one")
		logsFlags.Parse(os.Args[2:])

		if *archiveFlag {
			archivePath, err := client.ArchiveLogs()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("archived to: %s\n", archivePath)
		} else {
			if err := client.GetLogs(); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		fmt.Fprintf(os.Stderr, "usage: %s <start|stop|i|ps|ls|tags|du|logs> [args...]\n", os.Args[0])
		os.Exit(1)
	}
}
