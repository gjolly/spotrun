package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/gjolly/spotrun/internal/config"
	"github.com/gjolly/spotrun/internal/monitor"
	"github.com/gjolly/spotrun/internal/provision"
	"github.com/gjolly/spotrun/internal/spotfinder"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 3 || os.Args[1] != "run" {
		return fmt.Errorf("usage: spotrun run [--debug-ssh] <config.yaml>")
	}

	var debugSSH bool
	var configPath string
	for _, arg := range os.Args[2:] {
		switch arg {
		case "--debug-ssh":
			debugSSH = true
		default:
			configPath = arg
		}
	}
	if configPath == "" {
		return fmt.Errorf("usage: spotrun run [--debug-ssh] <config.yaml>")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	registryUser := os.Getenv("SPOTRUN_REGISTRY_USER")
	registryToken := os.Getenv("SPOTRUN_REGISTRY_TOKEN")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Searching %d regions for cheapest spot instance...\n", len(cfg.Regions))

	candidates, err := spotfinder.FindAll(ctx, cfg)
	if err != nil {
		return fmt.Errorf("finding spot instance: %w", err)
	}

	fmt.Printf("Found %d candidate(s). Provisioning cheapest available...\n\n", len(candidates))

	var instance *provision.Instance
	var cleanup func()
	for i, candidate := range candidates {
		fmt.Printf("[%d/%d] Trying %s in %s (%s) — $%.4f/hr\n",
			i+1, len(candidates), candidate.InstanceType, candidate.Region, candidate.AZ, candidate.Price)

		instance, cleanup, err = provision.Launch(ctx, cfg, &candidate, registryUser, registryToken)
		if err != nil {
			if provision.IsInsufficientCapacity(err) {
				fmt.Printf("  insufficient capacity, trying next candidate...\n")
				continue
			}
			return fmt.Errorf("provisioning instance: %w", err)
		}
		fmt.Printf("  Type:   %s\n", candidate.InstanceType)
		fmt.Printf("  Region: %s\n", candidate.Region)
		fmt.Printf("  AZ:     %s\n", candidate.AZ)
		fmt.Printf("  vCPUs:  %d\n", candidate.VCPUs)
		fmt.Printf("  RAM:    %.1f GiB\n", float64(candidate.MemoryMiB)/1024)
		fmt.Printf("  Price:  $%.4f/hr\n", candidate.Price)
		if candidate.HasLocalNVMe {
			fmt.Printf("  NVMe:   %d GiB\n", candidate.LocalNVMeGiB)
		}
		fmt.Println()
		break
	}
	if instance == nil {
		return fmt.Errorf("no spot capacity available across all %d candidate(s)", len(candidates))
	}

	// Wrap cleanup in sync.Once so it's safe to call from both defer and the signal goroutine.
	var once sync.Once
	safeCleanup := func() { once.Do(cleanup) }
	defer safeCleanup()

	// Belt-and-suspenders: if anything blocks after this point and a signal arrives,
	// run cleanup directly from the signal goroutine rather than waiting for defer.
	go func() {
		<-ctx.Done()
		fmt.Println("\nInterrupted — cleaning up instance...")
		safeCleanup()
	}()

	fmt.Printf("Instance %s ready at %s\n\n", instance.ID, instance.PublicIP)

	if debugSSH {
		fmt.Println("--- SSH ACCESS ---")
		fmt.Print(instance.SSHPrivateKeyPEM)
		fmt.Printf("# Save the key above to a file, then:\n")
		fmt.Printf("# chmod 600 /tmp/spotrun.pem\n")
		fmt.Printf("# ssh -i /tmp/spotrun.pem %s@%s\n", instance.SSHUser, instance.PublicIP)
		fmt.Println("------------------")
		fmt.Println()
	}
	fmt.Println("Streaming workload logs:")
	fmt.Println(strings.Repeat("-", 60))

	if err := monitor.Run(ctx, cfg, instance); err != nil {
		if context.Cause(ctx) != nil {
			// User interrupted — cleanup already running, exit cleanly.
			return nil
		}
		fmt.Fprintf(os.Stderr, "workload error: %v\n", err)
		fmt.Printf("\nResults saved to %s\n", cfg.Output.LocalDir)
		return err
	}

	fmt.Printf("\nResults saved to %s\n", cfg.Output.LocalDir)
	return nil
}
