package monitor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gjolly/spotrun/internal/config"
	"github.com/gjolly/spotrun/internal/provision"
	"golang.org/x/crypto/ssh"
)

func Run(ctx context.Context, cfg *config.Config, instance *provision.Instance) error {
	sshClient, err := connectSSH(ctx, instance)
	if err != nil {
		return fmt.Errorf("connecting SSH: %w", err)
	}
	defer sshClient.Close()

	done := make(chan int, 1)

	// Log streaming goroutine
	logSession, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("opening log session: %w", err)
	}
	logSession.Stdout = os.Stdout
	logSession.Stderr = os.Stderr

	if err := logSession.Start("tail -F /var/log/spotrun/output.log 2>/dev/null"); err != nil {
		logSession.Close()
		return fmt.Errorf("starting log tail: %w", err)
	}

	go func() {
		_ = logSession.Wait()
	}()

	// Completion polling goroutine
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sess, err := sshClient.NewSession()
				if err != nil {
					continue
				}
				out, err := sess.Output("cat /var/log/spotrun/exitcode 2>/dev/null")
				sess.Close()
				if err != nil || len(strings.TrimSpace(string(out))) == 0 {
					continue
				}
				code, err := strconv.Atoi(strings.TrimSpace(string(out)))
				if err != nil {
					continue
				}
				done <- code
				return
			}
		}
	}()

	var exitCode int
	select {
	case exitCode = <-done:
	case <-ctx.Done():
		_ = logSession.Signal(ssh.SIGTERM)
		logSession.Close()
		return ctx.Err()
	}

	_ = logSession.Signal(ssh.SIGTERM)
	logSession.Close()

	// Download artifacts regardless of exit code
	fmt.Println("\nDownloading artifacts...")
	if err := downloadArtifacts(sshClient); err != nil {
		fmt.Printf("warning: downloading artifacts: %v\n", err)
	}

	if exitCode != 0 {
		return fmt.Errorf("workload exited with code %d", exitCode)
	}

	return nil
}

func connectSSH(ctx context.Context, instance *provision.Instance) (*ssh.Client, error) {
	sshCfg := &ssh.ClientConfig{
		User:            instance.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(instance.PrivateKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // ephemeral instance
		Timeout:         10 * time.Second,
	}

	addr := instance.PublicIP + ":22"
	timeout := time.After(10 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("timed out waiting for SSH after 10 minutes")
		case <-ticker.C:
			client, err := ssh.Dial("tcp", addr, sshCfg)
			if err == nil {
				return client, nil
			}
			fmt.Printf("Waiting for SSH (%v)...\n", err)
		}
	}
}

func downloadArtifacts(sshClient *ssh.Client) error {
	sess, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("creating SSH session: %w", err)
	}
	defer sess.Close()

	localPath := fmt.Sprintf("spotrun-output-%d.tar.gz", time.Now().Unix())
	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("creating local tarball %s: %w", localPath, err)
	}
	defer f.Close()

	sess.Stdout = f

	if err := sess.Run("tar czf - -C /spotrun-output . 2>/dev/null"); err != nil {
		return fmt.Errorf("remote tar: %w", err)
	}

	fmt.Printf("artifacts saved to %s\n", localPath)
	return nil
}
