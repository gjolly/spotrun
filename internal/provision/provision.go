package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/smithy-go"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/gjolly/spotrun/internal/cloudinit"
	"github.com/gjolly/spotrun/internal/config"
	"github.com/gjolly/spotrun/internal/spotfinder"
	"golang.org/x/crypto/ssh"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type Instance struct {
	ID         string
	PublicIP   string
	Region     string
	PrivateKey ssh.Signer
	SSHUser    string
}

func Launch(ctx context.Context, cfg *config.Config, candidate *spotfinder.Candidate, registryUser, registryToken string) (*Instance, func(), error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(candidate.Region))
	if err != nil {
		return nil, nil, fmt.Errorf("loading AWS config: %w", err)
	}
	client := ec2.NewFromConfig(awsCfg)

	// Generate SSH key pair
	signer, pubKeyLine, err := generateSSHKeyPair()
	if err != nil {
		return nil, nil, fmt.Errorf("generating SSH key: %w", err)
	}

	// Find AMI
	amiID, err := findAMI(ctx, client, candidate.Arch)
	if err != nil {
		return nil, nil, fmt.Errorf("finding AMI: %w", err)
	}

	// Get my public IP
	myIP, err := getMyPublicIP()
	if err != nil {
		return nil, nil, fmt.Errorf("getting public IP: %w", err)
	}

	// Create security group
	sgID, err := createSecurityGroup(ctx, client, myIP)
	if err != nil {
		return nil, nil, fmt.Errorf("creating security group: %w", err)
	}

	// Generate user data
	userData, err := cloudinit.Generate(cloudinit.Params{
		SSHPublicKey:  pubKeyLine,
		Image:         cfg.Workload.Image,
		OutputDir:     cfg.Workload.OutputDir,
		Env:           cfg.Workload.Env,
		StorageType:   cfg.Requirements.Storage.Type,
		RegistryUser:  registryUser,
		RegistryToken: registryToken,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("generating user data: %w", err)
	}

	// Determine root volume size
	rootVolGiB := int32(30)
	if cfg.Requirements.Storage.Type != "nvme" {
		rootVolGiB = max32(30, int32(cfg.Requirements.Storage.SizeGiB)+10)
	}

	// Launch instance
	runOut, err := client.RunInstances(ctx, &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		ImageId:      aws.String(amiID),
		InstanceType: types.InstanceType(candidate.InstanceType),
		UserData:     aws.String(userData),
		Placement: &types.Placement{
			AvailabilityZone: aws.String(candidate.AZ),
		},
		SecurityGroupIds: []string{sgID},
		InstanceMarketOptions: &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType: types.SpotInstanceTypeOneTime,
			},
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize: aws.Int32(rootVolGiB),
					VolumeType: types.VolumeTypeGp3,
				},
			},
		},
	})
	if err != nil {
		// Clean up security group if launch fails
		_, _ = client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(sgID)})
		return nil, nil, fmt.Errorf("launching instance: %w", err)
	}

	instanceID := aws.ToString(runOut.Instances[0].InstanceId)

	cleanup := func() {
		cleanupCtx := context.Background()
		_, err := client.TerminateInstances(cleanupCtx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			fmt.Printf("warning: terminating instance %s: %v\n", instanceID, err)
		} else {
			fmt.Printf("Instance %s terminated.\n", instanceID)
		}
		// Wait for SG to detach
		time.Sleep(15 * time.Second)
		_, err = client.DeleteSecurityGroup(cleanupCtx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(sgID),
		})
		if err != nil {
			fmt.Printf("warning: deleting security group %s: %v\n", sgID, err)
		} else {
			fmt.Printf("Security group %s deleted.\n", sgID)
		}
	}

	// Wait for public IP
	publicIP, err := waitForPublicIP(ctx, client, instanceID)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("waiting for public IP: %w", err)
	}

	return &Instance{
		ID:         instanceID,
		PublicIP:   publicIP,
		Region:     candidate.Region,
		PrivateKey: signer,
		SSHUser:    "ubuntu",
	}, cleanup, nil
}

func generateSSHKeyPair() (ssh.Signer, string, error) {
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}

	signer, err := ssh.NewSignerFromKey(privKey)
	if err != nil {
		return nil, "", err
	}

	pubKey := signer.PublicKey()
	pubKeyLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubKey)))

	return signer, pubKeyLine, nil
}

func findAMI(ctx context.Context, client *ec2.Client, arch string) (string, error) {
	awsArch := "amd64"
	if arch == "arm64" {
		awsArch = "arm64"
	}

	namePattern := fmt.Sprintf("ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-%s-server-*", awsArch)

	out, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"099720109477"},
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{namePattern}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", err
	}

	if len(out.Images) == 0 {
		return "", fmt.Errorf("no Ubuntu 24.04 AMI found for arch %s", awsArch)
	}

	// Sort by CreationDate descending — pick most recent
	best := out.Images[0]
	for _, img := range out.Images[1:] {
		if aws.ToString(img.CreationDate) > aws.ToString(best.CreationDate) {
			best = img
		}
	}

	return aws.ToString(best.ImageId), nil
}

func getMyPublicIP() (string, error) {
	resp, err := http.Get("https://checkip.amazonaws.com")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(body)), nil
}

func createSecurityGroup(ctx context.Context, client *ec2.Client, myIP string) (string, error) {
	sgName := fmt.Sprintf("spotrun-%d", time.Now().Unix())

	createOut, err := client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("Temporary spotrun SSH access"),
	})
	if err != nil {
		return "", fmt.Errorf("creating security group: %w", err)
	}

	sgID := aws.ToString(createOut.GroupId)

	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String(myIP + "/32")},
				},
			},
		},
	})
	if err != nil {
		_, _ = client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(sgID)})
		return "", fmt.Errorf("authorizing SSH ingress: %w", err)
	}

	return sgID, nil
}

func waitForPublicIP(ctx context.Context, client *ec2.Client, instanceID string) (string, error) {
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			return "", fmt.Errorf("timed out waiting for public IP after 5 minutes")
		case <-ticker.C:
			out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
				InstanceIds: []string{instanceID},
			})
			if err != nil {
				return "", err
			}
			if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
				ip := aws.ToString(out.Reservations[0].Instances[0].PublicIpAddress)
				if ip != "" {
					return ip, nil
				}
			}
		}
	}
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// IsInsufficientCapacity returns true when RunInstances failed because AWS has
// no available capacity for the requested instance type in the given AZ.
func IsInsufficientCapacity(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "InsufficientInstanceCapacity"
}
