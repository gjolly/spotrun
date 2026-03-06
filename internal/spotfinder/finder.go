package spotfinder

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/gjolly/spotrun/internal/config"
)

type Candidate struct {
	InstanceType string
	Region       string
	AZ           string
	Price        float64
	VCPUs        int32
	MemoryMiB    int64
	Arch         string
	HasLocalNVMe bool
	LocalNVMeGiB int64
}

// FindAll returns all matching spot candidates across all regions, sorted by price ascending.
func FindAll(ctx context.Context, cfg *config.Config) ([]Candidate, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var allCandidates []Candidate

	for _, region := range cfg.Regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			candidates, err := findInRegion(ctx, cfg, region)
			if err != nil {
				fmt.Printf("warning: region %s: %v\n", region, err)
				return
			}
			mu.Lock()
			allCandidates = append(allCandidates, candidates...)
			mu.Unlock()
		}(region)
	}

	wg.Wait()

	if len(allCandidates) == 0 {
		return nil, fmt.Errorf("no spot instances found matching requirements across all regions")
	}

	// Filter by max price
	if cfg.Spot.MaxPriceUSDPerHour > 0 {
		filtered := allCandidates[:0]
		for _, c := range allCandidates {
			if c.Price <= cfg.Spot.MaxPriceUSDPerHour {
				filtered = append(filtered, c)
			}
		}
		allCandidates = filtered
	}

	if len(allCandidates) == 0 {
		return nil, fmt.Errorf("no spot instances found within price limit of $%.2f/hr", cfg.Spot.MaxPriceUSDPerHour)
	}

	sort.Slice(allCandidates, func(i, j int) bool {
		return allCandidates[i].Price < allCandidates[j].Price
	})

	return allCandidates, nil
}

func findInRegion(ctx context.Context, cfg *config.Config, region string) ([]Candidate, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := ec2.NewFromConfig(awsCfg)

	instanceTypes, err := getMatchingInstanceTypes(ctx, client, cfg)
	if err != nil {
		return nil, fmt.Errorf("getting instance types: %w", err)
	}

	if len(instanceTypes) == 0 {
		return nil, nil
	}

	candidates, err := getSpotPrices(ctx, client, instanceTypes, region)
	if err != nil {
		return nil, fmt.Errorf("getting spot prices: %w", err)
	}

	sqClient := servicequotas.NewFromConfig(awsCfg)
	available, err := getQuotaAvailable(ctx, sqClient, client, candidates)
	if err != nil {
		fmt.Printf("warning: region %s: quota check failed, skipping filter: %v\n", region, err)
	} else {
		filtered := candidates[:0]
		for _, c := range candidates {
			code := spotQuotaCode(c.InstanceType)
			if avail, ok := available[code]; ok && c.VCPUs > avail {
				continue
			}
			filtered = append(filtered, c)
		}
		candidates = filtered
	}

	return candidates, nil
}

// spotQuotaCode returns the EC2 spot vCPU quota code for a given instance type.
func spotQuotaCode(instanceType string) string {
	// Extract the alphabetic family prefix (e.g. "m5.2xlarge" → "m", "inf1.xlarge" → "inf")
	prefix := strings.ToLower(instanceType)
	end := strings.IndexFunc(prefix, func(r rune) bool { return r < 'a' || r > 'z' })
	if end > 0 {
		prefix = prefix[:end]
	}

	switch prefix {
	case "g", "vt":
		return "L-3819A6DF"
	case "p":
		return "L-7212CCBC"
	case "f":
		return "L-88CF9481"
	case "x":
		return "L-E3A00192"
	case "dl":
		return "L-85EED4F7"
	case "inf":
		return "L-B5D1601B"
	case "trn":
		return "L-6B0D517C"
	default:
		return "L-34B43A08"
	}
}

// getQuotaAvailable returns a map of quota code → available vCPUs (limit − used).
func getQuotaAvailable(ctx context.Context, sqClient *servicequotas.Client, ec2Client *ec2.Client, candidates []Candidate) (map[string]int32, error) {
	// Collect distinct quota codes for the candidate instance types
	codes := make(map[string]struct{})
	for _, c := range candidates {
		codes[spotQuotaCode(c.InstanceType)] = struct{}{}
	}

	// Fetch quota limits
	limits := make(map[string]int32)
	for code := range codes {
		out, err := sqClient.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
			ServiceCode: aws.String("ec2"),
			QuotaCode:   aws.String(code),
		})
		if err != nil {
			return nil, fmt.Errorf("GetServiceQuota %s: %w", code, err)
		}
		limits[code] = int32(aws.ToFloat64(out.Quota.Value))
	}

	// Count currently used spot vCPUs per quota code
	used := make(map[string]int32)
	paginator := ec2.NewDescribeSpotInstanceRequestsPaginator(ec2Client, &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []types.Filter{
			{Name: aws.String("state"), Values: []string{"active", "open"}},
		},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("DescribeSpotInstanceRequests: %w", err)
		}
		for _, req := range page.SpotInstanceRequests {
			it := string(req.LaunchSpecification.InstanceType)
			if it == "" {
				continue
			}
			code := spotQuotaCode(it)
			if _, tracked := limits[code]; !tracked {
				continue
			}
			// Look up vCPU count for this instance type
			itOut, err := ec2Client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
				InstanceTypes: []types.InstanceType{req.LaunchSpecification.InstanceType},
			})
			if err != nil || len(itOut.InstanceTypes) == 0 {
				continue
			}
			vcpus := aws.ToInt32(itOut.InstanceTypes[0].VCpuInfo.DefaultVCpus)
			used[code] += vcpus
		}
	}

	// Compute available = limit − used, clamped to 0
	available := make(map[string]int32, len(limits))
	for code, limit := range limits {
		avail := limit - used[code]
		if avail < 0 {
			avail = 0
		}
		available[code] = avail
	}
	return available, nil
}

type instanceInfo struct {
	vcpus        int32
	memoryMiB    int64
	arch         string
	hasLocalNVMe bool
	localNVMeGiB int64
}

func getMatchingInstanceTypes(ctx context.Context, client *ec2.Client, cfg *config.Config) (map[string]instanceInfo, error) {
	filters := []types.Filter{
		{Name: aws.String("current-generation"), Values: []string{"true"}},
		{Name: aws.String("supported-virtualization-type"), Values: []string{"hvm"}},
	}

	if cfg.Requirements.Arch != "any" {
		awsArch := "x86_64"
		if cfg.Requirements.Arch == "arm64" {
			awsArch = "arm64"
		}
		filters = append(filters, types.Filter{
			Name:   aws.String("processor-info.supported-architecture"),
			Values: []string{awsArch},
		})
	}

	paginator := ec2.NewDescribeInstanceTypesPaginator(client, &ec2.DescribeInstanceTypesInput{
		Filters: filters,
	})

	result := make(map[string]instanceInfo)
	memMin := int64(cfg.Requirements.MemoryGiBMin * 1024)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, it := range page.InstanceTypes {
			if it.VCpuInfo == nil || it.MemoryInfo == nil {
				continue
			}

			vcpus := aws.ToInt32(it.VCpuInfo.DefaultVCpus)
			memMiB := aws.ToInt64(it.MemoryInfo.SizeInMiB)

			if vcpus < cfg.Requirements.VCPUsMin || memMiB < memMin {
				continue
			}

			arch := "x86_64"
			if len(it.ProcessorInfo.SupportedArchitectures) > 0 {
				arch = string(it.ProcessorInfo.SupportedArchitectures[0])
			}

			info := instanceInfo{
				vcpus:     vcpus,
				memoryMiB: memMiB,
				arch:      arch,
			}

			// Check NVMe storage
			if cfg.Requirements.Storage.Type == "nvme" {
				if it.InstanceStorageInfo == nil ||
					it.InstanceStorageInfo.NvmeSupport == types.EphemeralNvmeSupportUnsupported {
					continue
				}

				var totalNVMeGiB int64
				for _, disk := range it.InstanceStorageInfo.Disks {
					totalNVMeGiB += int64(aws.ToInt32(disk.Count)) * aws.ToInt64(disk.SizeInGB)
				}

				if totalNVMeGiB < cfg.Requirements.Storage.SizeGiB {
					continue
				}

				info.hasLocalNVMe = true
				info.localNVMeGiB = totalNVMeGiB
			} else if it.InstanceStorageInfo != nil &&
				it.InstanceStorageInfo.NvmeSupport != types.EphemeralNvmeSupportUnsupported {
				var totalNVMeGiB int64
				for _, disk := range it.InstanceStorageInfo.Disks {
					totalNVMeGiB += int64(aws.ToInt32(disk.Count)) * aws.ToInt64(disk.SizeInGB)
				}
				info.hasLocalNVMe = true
				info.localNVMeGiB = totalNVMeGiB
			}

			result[string(it.InstanceType)] = info
		}
	}

	return result, nil
}

func getSpotPrices(ctx context.Context, client *ec2.Client, instanceTypes map[string]instanceInfo, region string) ([]Candidate, error) {
	typeList := make([]types.InstanceType, 0, len(instanceTypes))
	for t := range instanceTypes {
		typeList = append(typeList, types.InstanceType(t))
	}

	now := time.Now()
	paginator := ec2.NewDescribeSpotPriceHistoryPaginator(client, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       typeList,
		ProductDescriptions: []string{"Linux/UNIX"},
		StartTime:           &now,
	})

	// Track best price per (instanceType, AZ)
	type key struct {
		instanceType string
		az           string
	}
	best := make(map[key]float64)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, entry := range page.SpotPriceHistory {
			price, err := strconv.ParseFloat(aws.ToString(entry.SpotPrice), 64)
			if err != nil {
				continue
			}

			k := key{
				instanceType: string(entry.InstanceType),
				az:           aws.ToString(entry.AvailabilityZone),
			}

			if existing, ok := best[k]; !ok || price < existing {
				best[k] = price
			}
		}
	}

	var candidates []Candidate
	for k, price := range best {
		info := instanceTypes[k.instanceType]
		candidates = append(candidates, Candidate{
			InstanceType: k.instanceType,
			Region:       region,
			AZ:           k.az,
			Price:        price,
			VCPUs:        info.vcpus,
			MemoryMiB:    info.memoryMiB,
			Arch:         info.arch,
			HasLocalNVMe: info.hasLocalNVMe,
			LocalNVMeGiB: info.localNVMeGiB,
		})
	}

	return candidates, nil
}
