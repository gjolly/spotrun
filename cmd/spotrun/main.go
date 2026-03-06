package main

import (
	"context"
	"fmt"
	"os"
	"strings"

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
		return fmt.Errorf("usage: spotrun run <config.yaml>")
	}

	configPath := os.Args[2]

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	registryUser := os.Getenv("SPOTRUN_REGISTRY_USER")
	registryToken := os.Getenv("SPOTRUN_REGISTRY_TOKEN")

	ctx := context.Background()

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
	defer cleanup()

	fmt.Printf("Instance %s ready at %s\n\n", instance.ID, instance.PublicIP)
	fmt.Println("Streaming workload logs:")
	fmt.Println(strings.Repeat("-", 60))

	if err := monitor.Run(ctx, cfg, instance); err != nil {
		fmt.Fprintf(os.Stderr, "workload error: %v\n", err)
		fmt.Printf("\nResults saved to %s\n", cfg.Output.LocalDir)
		return err
	}

	fmt.Printf("\nResults saved to %s\n", cfg.Output.LocalDir)
	return nil
}
