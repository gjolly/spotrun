# spotrun

Run OCI-image workloads on cheap AWS spot instances. Finds the lowest-priced instance matching your compute requirements, provisions it, streams logs live, downloads output artifacts, and terminates the instance on completion.

## Prerequisites

- Go 1.21+
- AWS credentials configured (environment variables, `~/.aws/credentials`, or IAM role)
- The AWS IAM principal needs: `ec2:DescribeInstanceTypes`, `ec2:DescribeSpotPriceHistory`, `ec2:DescribeImages`, `ec2:RunInstances`, `ec2:DescribeInstances`, `ec2:TerminateInstances`, `ec2:CreateSecurityGroup`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:DeleteSecurityGroup`

## Install

```bash
go install github.com/gjolly/spotrun/cmd/spotrun@latest
```

Or build from source:

```bash
git clone https://github.com/gjolly/spotrun
cd spotrun
go build ./cmd/spotrun
```

## Usage

```bash
spotrun run config.yaml
```

spotrun will:
1. Search all configured regions in parallel for the cheapest matching spot instance
2. Launch the instance with a temporary SSH key and security group
3. Pull and run your container, streaming logs to the terminal
4. Download everything written to `workload.output_dir` into `output.local_dir`
5. Terminate the instance and delete the security group

## Configuration

```yaml
# Regions to search (picks cheapest across all of them)
regions:
  - us-east-1
  - us-west-2
  - eu-west-1

requirements:
  vcpus_min: 32           # minimum vCPUs
  memory_gib_min: 64      # minimum RAM in GiB
  arch: amd64             # amd64 | arm64 | any
  storage:
    type: nvme            # nvme | ebs | any
    size_gib: 500         # for nvme: minimum local NVMe GiB
                          # for ebs/any: extra GiB added to the root volume

workload:
  image: ghcr.io/myorg/mybuild:latest
  output_dir: /output     # directory inside the container to download after the run
  env:
    BUILD_JOBS: "64"      # environment variables passed to the container

spot:
  max_price_usd_per_hour: 2.00  # optional price cap; omit for no limit

output:
  local_dir: ./output     # local directory where artifacts are saved (default: ./output)
```

### Storage types

| `type` | Behavior |
|--------|----------|
| `nvme` | Requires local NVMe storage ≥ `size_gib`. The first non-root NVMe device is formatted and mounted at the container output path. |
| `ebs`  | No local NVMe required. Root EBS volume is sized to `size_gib + 10` GiB. |
| `any`  | No storage constraint. Root EBS volume sizing same as `ebs`. |

## Private registries

Set these environment variables before running:

```bash
export SPOTRUN_REGISTRY_USER=myuser
export SPOTRUN_REGISTRY_TOKEN=mytoken
spotrun run config.yaml
```

spotrun will run `docker login` on the instance before pulling the image. The credentials are embedded in the instance user-data — only use this with private, short-lived instances (which is the default).

## Examples

### Build the Linux kernel

`examples/kernel-build/` contains a `Containerfile` that fetches the latest mainline kernel from kernel.org at image build time and compiles it when the container runs.

```bash
# Copy your kernel config into the build context
cp /boot/config-$(uname -r) examples/kernel-build/.config

# Build the image (downloads kernel source)
docker build -f examples/kernel-build/Containerfile -t my-kernel-build examples/kernel-build/

# Push to a registry, then run with spotrun
spotrun run examples/kernel-build/spotrun.yaml
```

Output artifacts (`vmlinuz`, `System.map`, `.config`) are written to `/output` inside the container and downloaded to `./output` locally.

## What happens on spot interruption

The run fails immediately and reports the error. Artifacts written so far are still downloaded. V1 does not retry.
