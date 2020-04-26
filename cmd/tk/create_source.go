package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"strings"
	"text/template"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
)

var createSourceCmd = &cobra.Command{
	Use:   "source [name]",
	Short: "Create source resource",
	Long: `
The create source command generates a source.fluxcd.io resource and waits for it to sync.
For Git over SSH, host and SSH keys are automatically generated.`,
	Example: `  # Create a gitrepository.source.fluxcd.io for a public repository
  create source podinfo --git-url https://github.com/stefanprodan/podinfo-deploy --git-branch master

  # Create a gitrepository.source.fluxcd.io that syncs tags based on a semver range
  create source podinfo --git-url https://github.com/stefanprodan/podinfo-deploy  --git-semver=">=0.0.1-rc.1 <0.1.0"

  # Create a gitrepository.source.fluxcd.io with SSH authentication
  create source podinfo --git-url ssh://git@github.com/stefanprodan/podinfo-deploy

  # Create a gitrepository.source.fluxcd.io with basic authentication
  create source podinfo --git-url https://github.com/stefanprodan/podinfo-deploy -u username -p password
`,
	RunE: createSourceCmdRun,
}

var (
	sourceGitURL    string
	sourceGitBranch string
	sourceGitSemver string
	sourceUsername  string
	sourcePassword  string
)

func init() {
	createSourceCmd.Flags().StringVar(&sourceGitURL, "git-url", "", "git address, e.g. ssh://git@host/org/repository")
	createSourceCmd.Flags().StringVar(&sourceGitBranch, "git-branch", "master", "git branch")
	createSourceCmd.Flags().StringVar(&sourceGitSemver, "git-semver", "", "git tag semver range")
	createSourceCmd.Flags().StringVarP(&sourceUsername, "username", "u", "", "basic authentication username")
	createSourceCmd.Flags().StringVarP(&sourcePassword, "password", "p", "", "basic authentication password")

	createCmd.AddCommand(createSourceCmd)
}

func createSourceCmdRun(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("source name is required")
	}
	name := args[0]

	if sourceGitURL == "" {
		return fmt.Errorf("git-url is required")
	}

	tmpDir, err := ioutil.TempDir("", name)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	u, err := url.Parse(sourceGitURL)
	if err != nil {
		return fmt.Errorf("git URL parse failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	withAuth := false
	if strings.HasPrefix(sourceGitURL, "ssh") {
		if err := generateSSH(ctx, name, u.Host, tmpDir); err != nil {
			return err
		}
		withAuth = true
	} else if sourceUsername != "" && sourcePassword != "" {
		if err := generateBasicAuth(ctx, name); err != nil {
			return err
		}
		withAuth = true
	}

	logAction("generating source %s in %s namespace", name, namespace)

	t, err := template.New("tmpl").Parse(gitSource)
	if err != nil {
		return fmt.Errorf("template parse error: %w", err)
	}

	source := struct {
		Name      string
		Namespace string
		URL       string
		Branch    string
		Semver    string
		Interval  string
		WithAuth  bool
	}{
		Name:      name,
		Namespace: namespace,
		URL:       sourceGitURL,
		Branch:    sourceGitBranch,
		Semver:    sourceGitSemver,
		Interval:  interval,
		WithAuth:  withAuth,
	}

	var data bytes.Buffer
	writer := bufio.NewWriter(&data)
	if err := t.Execute(writer, source); err != nil {
		return fmt.Errorf("template execution failed: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("source flush failed: %w", err)
	}

	if verbose {
		fmt.Print(data.String())
	}

	command := fmt.Sprintf("echo '%s' | kubectl apply -f-", data.String())
	if _, err := utils.execCommand(ctx, ModeStderrOS, command); err != nil {
		return fmt.Errorf("source apply failed")
	}

	logAction("waiting for source sync")
	command = fmt.Sprintf("kubectl -n %s wait gitrepository/%s --for=condition=ready --timeout=1m",
		namespace, name)
	if _, err := utils.execCommand(ctx, ModeStderrOS, command); err != nil {
		return fmt.Errorf("source sync failed")
	}
	logSuccess("source %s is ready", name)
	return nil
}

func generateBasicAuth(ctx context.Context, name string) error {
	logAction("saving credentials")
	credentials := fmt.Sprintf("--from-literal=username='%s' --from-literal=password='%s'",
		sourceUsername, sourcePassword)
	secret := fmt.Sprintf("kubectl -n %s create secret generic %s %s --dry-run=client -oyaml | kubectl apply -f-",
		namespace, name, credentials)
	if _, err := utils.execCommand(ctx, ModeOS, secret); err != nil {
		return fmt.Errorf("kubectl create secret failed")
	}
	return nil
}

func generateSSH(ctx context.Context, name, host, tmpDir string) error {
	logAction("generating host key for %s", host)

	command := fmt.Sprintf("ssh-keyscan %s > %s/known_hosts", host, tmpDir)
	if _, err := utils.execCommand(ctx, ModeStderrOS, command); err != nil {
		return fmt.Errorf("ssh-keyscan failed")
	}

	logAction("generating deploy key")

	command = fmt.Sprintf("ssh-keygen -b 2048 -t rsa -f %s/identity -q -N \"\"", tmpDir)
	if _, err := utils.execCommand(ctx, ModeStderrOS, command); err != nil {
		return fmt.Errorf("ssh-keygen failed")
	}

	command = fmt.Sprintf("cat %s/identity.pub", tmpDir)
	if deployKey, err := utils.execCommand(ctx, ModeCapture, command); err != nil {
		return fmt.Errorf("unable to read identity.pub: %w", err)
	} else {
		fmt.Print(deployKey)
	}

	prompt := promptui.Prompt{
		Label:     "Have you added the deploy key to your repository",
		IsConfirm: true,
	}
	if _, err := prompt.Run(); err != nil {
		logFailure("aborting")
		os.Exit(1)
	}

	logAction("saving deploy key")
	files := fmt.Sprintf("--from-file=%s/identity --from-file=%s/identity.pub --from-file=%s/known_hosts",
		tmpDir, tmpDir, tmpDir)
	secret := fmt.Sprintf("kubectl -n %s create secret generic %s %s --dry-run=client -oyaml | kubectl apply -f-",
		namespace, name, files)
	if _, err := utils.execCommand(ctx, ModeOS, secret); err != nil {
		return fmt.Errorf("create secret failed")
	}
	return nil
}

var gitSource = `---
apiVersion: source.fluxcd.io/v1alpha1
kind: GitRepository
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  interval: {{.Interval}}
  url: {{.URL}}
  ref:
{{- if .Semver }}
    semver: "{{.Semver}}"
{{- else }}
    branch: {{.Branch}}
{{- end }}
{{- if .WithAuth }}
  secretRef:
    name: {{.Name}}
{{- end }}
`