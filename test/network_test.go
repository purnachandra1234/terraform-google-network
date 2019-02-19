package test

import (
	"fmt"

	"github.com/gruntwork-io/terratest/modules/gcp"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/ssh"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/gruntwork-io/terratest/modules/test-structure"
	"path/filepath"
	"strings"
	"testing"
)

// TODO: Add test stages
func TestNetworkManagement(t *testing.T) {
	t.Parallel()

	testFolder := test_structure.CopyTerraformFolderToTemp(t, "..", "examples")
	terraformModulePath := filepath.Join(testFolder, "network-management")

	project := gcp.GetGoogleProjectIDFromEnvVar(t)
	region := gcp.GetRandomRegion(t, project, nil, nil)
	terratestOptions := createNetworkManagementTerraformOptions(t, project, region, terraformModulePath)
	defer terraform.Destroy(t, terratestOptions)

	terraform.InitAndApply(t, terratestOptions)

	/*
	Test Outputs
	*/
	// Guarantee that we see expected values from state
	var stateValues = []struct {
		outputKey  string
		expectedValue string

		// With two string insertion points
		message string
	}{
		// Testing the cidr block itself is just reading the value out of the Terraform config;
		// by testing the gateway addresses, we've confirmed that the API had allocated the correct
		// block, although not necessarily the correct size.
		{"public_subnetwork_gateway", "10.0.0.1", "expected a public gateway of %s but saw %s"},
		{"private_subnetwork_gateway", "10.0.16.1", "expected a public gateway of %s but saw %s"},

		// Network tags as interpolation targets
		{"public", "public", "expected a tag of %s but saw %s"},
		{"private", "private", "expected a tag of %s but saw %s"},
		{"private_persistence", "private-persistence", "expected a tag of %s but saw %s"},
	}

	for _, tt := range stateValues {
		t.Run(tt.outputKey, func(t *testing.T) {
			value, err := terraform.OutputE(t, terratestOptions, tt.outputKey)
			if err != nil {
				t.Errorf("could not find %s in outputs: %s", tt.outputKey, err)
			}

			if value != tt.expectedValue {
				t.Errorf(tt.message, tt.expectedValue, value)
			}
		})
	}

	/*
	Test SSH
	*/
	external := FetchFromOutput(t, terratestOptions, project, "instance_default_network")
	publicWithIp := FetchFromOutput(t, terratestOptions, project, "instance_public_with_ip")
	publicWithoutIp := FetchFromOutput(t, terratestOptions, project, "instance_public_without_ip")

	keyPair := ssh.GenerateRSAKeyPair(t, 2048)
	sshUsername := "terratest"

	// Attach the SSH Key to each instances so we can access them at will later
	for _, v := range []*gcp.Instance{external, publicWithIp, publicWithoutIp} {
		v.AddSshKey(t, sshUsername, keyPair.PublicKey)
	}

	// "external internet" settings pulled from the instance in the default network
	externalHost := ssh.Host{
		Hostname:    external.GetPublicIp(t),
		SshKeyPair:  keyPair,
		SshUserName: sshUsername,
	}

	// We can SSH to the public instance w/ an IP
	publicWithIpHost := ssh.Host{
		Hostname:    publicWithIp.GetPublicIp(t),
		SshKeyPair:  keyPair,
		SshUserName: sshUsername,
	}

	// The public instance w/ no IP can't be accessed directly but can through a bastion
	if _, err := publicWithoutIp.GetPublicIpE(t); err == nil {
		t.Errorf("Found an external IP on %s when it should have had none", publicWithoutIp.Name)
	}

	publicWithoutIpHost := ssh.Host{
		Hostname:    publicWithoutIp.Name,
		SshKeyPair:  keyPair,
		SshUserName: sshUsername,
	}

	sshChecks := []SSHCheck{
		{"public", func(t *testing.T) { testSSHOn1Host(t, ExpectSuccess, publicWithIpHost)} },
		{"public-no-ip", func(t *testing.T) { testSSHOn1Host(t, ExpectFailure, publicWithoutIpHost)} },
		{"public to external", func(t *testing.T) { testSSHOn2Hosts(t, ExpectSuccess, publicWithIpHost, externalHost)} },
		{"public to public-no-ip", func(t *testing.T) { testSSHOn2Hosts(t, ExpectSuccess, publicWithIpHost, publicWithoutIpHost)} },
	}

	// We need to run a series of parallel funcs inside a serial func in order to ensure that defer statements are ran after they've all completed
	t.Run("sshConnections", func(t *testing.T) {
		for _, check := range sshChecks {
			check := check // capture variable in local scope

			t.Run(check.Name, func(t *testing.T) {
				t.Parallel()
				check.Check(t)
			})
		}
	})
}

type SSHCheck struct {
	Name  string
	Check func(t *testing.T)
}

func testSSHOn1Host(t *testing.T, expectSuccess bool, hosts ...ssh.Host) {
	maxRetries := SSHMaxRetries
	if !expectSuccess {
		maxRetries = SSHMaxRetriesExpectError
	}

	_, err := retry.DoWithRetryE(t, "Attempting to SSH", maxRetries, SSHSleepBetweenRetries, func() (string, error) {
		output, err := ssh.CheckSshCommandE(t, hosts[0], fmt.Sprintf("echo '%s'", SSHEchoText))
		if err != nil {
			return "", err
		}

		if strings.TrimSpace(SSHEchoText) != strings.TrimSpace(output) {
			return "", fmt.Errorf("Expected: %s. Got: %s\n", SSHEchoText, output)
		}

		return "", nil
	})

	if err != nil && expectSuccess {
		t.Fatalf("Expected success but saw: %s", err)
	}

	if err == nil && !expectSuccess {
		t.Fatalf("Expected an error but saw none.")
	}
}

func testSSHOn2Hosts(t *testing.T, expectSuccess bool, hosts ...ssh.Host) {
	maxRetries := SSHMaxRetries
	if !expectSuccess {
		maxRetries = SSHMaxRetriesExpectError
	}

	_, err := retry.DoWithRetryE(t, "Attempting to SSH", maxRetries, SSHSleepBetweenRetries, func() (string, error) {
		output, err := ssh.CheckPrivateSshConnectionE(t, hosts[0], hosts[1], fmt.Sprintf("echo '%s'", SSHEchoText))
		if err != nil {
			return "", err
		}

		if strings.TrimSpace(SSHEchoText) != strings.TrimSpace(output) {
			return "", fmt.Errorf("Expected: %s. Got: %s\n", SSHEchoText, output)
		}

		return "", nil
	})

	if err != nil && expectSuccess {
		t.Fatalf("Expected success but saw: %s", err)
	}

	if err == nil && !expectSuccess {
		t.Fatalf("Expected an error but saw none.")
	}
}
