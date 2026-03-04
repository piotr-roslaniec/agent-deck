package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func handleHost(profile string, args []string) {
	_ = profile
	if len(args) == 0 {
		printHostHelp()
		os.Exit(1)
	}

	switch args[0] {
	case "list":
		handleHostList(args[1:])
	case "probe":
		handleHostProbe(args[1:])
	case "add":
		handleHostAdd(args[1:])
	case "remove":
		handleHostRemove(args[1:])
	case "help", "--help", "-h":
		printHostHelp()
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown host command: %s\n", args[0])
		printHostHelp()
		os.Exit(1)
	}
}

func printHostHelp() {
	fmt.Println("Usage: agent-deck host <command> [options]")
	fmt.Println()
	fmt.Println("Manage SSH hosts for remote sessions.")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  list                    Show hosts with live utilization")
	fmt.Println("  probe <name>            Detailed metrics for a host")
	fmt.Println("  add <name>              Add a new host")
	fmt.Println("  remove <name>           Remove a host")
	fmt.Println()
}

func handleHostList(args []string) {
	fs := flag.NewFlagSet("host list", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}

	config, err := session.LoadUserConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if len(config.Hosts) == 0 {
		if *jsonOutput {
			fmt.Println("[]")
		} else {
			fmt.Println("No hosts configured. Use 'agent-deck host add' to add one.")
		}
		return
	}

	// Probe all hosts
	ctx := context.Background()
	prober := &session.SSHHostProber{}
	results := session.ProbeAllHosts(ctx, config.Hosts, prober)
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	if *jsonOutput {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to format JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	// Table output
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSSH HOST\tDESCRIPTION\tCPU\tRAM\tDISK\tSTATUS")
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(w, "%s\t%s\t%s\t--\t--\t--\tunreachable\n",
				r.Name, r.Config.SSHHost, r.Config.Description)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.0f%%\tok\n",
				r.Name, r.Config.SSHHost, r.Config.Description,
				r.Metrics.CPUPercent, r.Metrics.RAMPercent, r.Metrics.DiskPercent)
		}
	}
	_ = w.Flush()
}

func handleHostProbe(args []string) {
	fs := flag.NewFlagSet("host probe", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}

	name := fs.Arg(0)
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: host name required")
		os.Exit(1)
	}

	config, err := session.LoadUserConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	hostCfg, ok := config.Hosts[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: host '%s' not found in config\n", name)
		os.Exit(1)
	}

	ctx := context.Background()
	prober := &session.SSHHostProber{}
	metrics, err := prober.Probe(ctx, hostCfg.SSHHost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error probing %s: %v\n", name, err)
		os.Exit(1)
	}

	if *jsonOutput {
		data, err := json.MarshalIndent(map[string]interface{}{
			"name":         name,
			"ssh_host":     hostCfg.SSHHost,
			"description":  hostCfg.Description,
			"cpu_percent":  metrics.CPUPercent,
			"ram_percent":  metrics.RAMPercent,
			"disk_percent": metrics.DiskPercent,
		}, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to format JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	fmt.Printf("Host: %s (%s)\n", name, hostCfg.SSHHost)
	if hostCfg.Description != "" {
		fmt.Printf("Description: %s\n", hostCfg.Description)
	}
	fmt.Printf("CPU:  %.1f%%\n", metrics.CPUPercent)
	fmt.Printf("RAM:  %.0f%%\n", metrics.RAMPercent)
	fmt.Printf("Disk: %.0f%%\n", metrics.DiskPercent)
}

func isValidHostName(name string) bool {
	return isValidRemoteName(name)
}

func handleHostAdd(args []string) {
	fs := flag.NewFlagSet("host add", flag.ExitOnError)
	sshHost := fs.String("ssh-host", "", "SSH destination (required)")
	description := fs.String("description", "", "Host description")
	defaultPath := fs.String("default-path", "", "Default remote working directory")
	test := fs.Bool("test", false, "Test SSH connectivity after adding")
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}

	name := fs.Arg(0)
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: host name required")
		fmt.Fprintln(os.Stderr, "Usage: agent-deck host add <name> --ssh-host <host>")
		os.Exit(1)
	}
	if !isValidHostName(name) {
		fmt.Fprintln(os.Stderr, "Error: host name must not contain spaces, slashes, dots, or colons")
		os.Exit(1)
	}
	if *sshHost == "" {
		fmt.Fprintln(os.Stderr, "Error: --ssh-host is required")
		os.Exit(1)
	}

	config, err := session.LoadUserConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if config.Hosts == nil {
		config.Hosts = make(map[string]session.HostConfig)
	}

	if _, exists := config.Hosts[name]; exists {
		fmt.Fprintf(os.Stderr, "Error: host '%s' already exists\n", name)
		os.Exit(1)
	}

	if *test {
		ctx := context.Background()
		prober := &session.SSHHostProber{}
		metrics, err := prober.Probe(ctx, *sshHost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Connection test FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Connected — CPU: %.0f%%, RAM: %.0f%%, Disk: %.0f%%\n",
			metrics.CPUPercent, metrics.RAMPercent, metrics.DiskPercent)
	}

	config.Hosts[name] = session.HostConfig{
		SSHHost:     *sshHost,
		Description: *description,
		DefaultPath: *defaultPath,
	}

	if err := session.SaveUserConfig(config); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Added host: %s (%s)\n", name, *sshHost)
}

func handleHostRemove(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: host name required")
		os.Exit(1)
	}

	name := args[0]

	config, err := session.LoadUserConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if config.Hosts == nil {
		fmt.Fprintf(os.Stderr, "Error: host '%s' not found\n", name)
		os.Exit(1)
	}

	if _, exists := config.Hosts[name]; !exists {
		fmt.Fprintf(os.Stderr, "Error: host '%s' not found\n", name)
		os.Exit(1)
	}

	delete(config.Hosts, name)

	if err := session.SaveUserConfig(config); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Removed host: %s\n", name)
}
