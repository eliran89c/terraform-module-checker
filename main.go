package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	log "github.com/sirupsen/logrus"
)

const (
	tempBinariesFolder = "/tmp/terraform-module-checker"
	defaultConstraints = ">1.0.0" // latest,
)

func init() {
	if os.Getenv("LOG_TYPE") == "JSON" {
		log.SetFormatter(&log.JSONFormatter{})
	}

	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		log.SetLevel(log.DebugLevel)
	}

	githubToken := os.Getenv("GH_TOKEN")
	if githubToken != "" {
		runCommand("git", "config", "--global", fmt.Sprintf("url.\"https://oauth2:%v@github.com\".insteadOf", githubToken), "\"ssh://git@github.com\"")
	}
}

type terraformInstaller struct {
	versions map[string]string
}

type VersionsFile struct {
	Terraform Terraform `hcl:"terraform,block"`
}

type Terraform struct {
	VersionConstraints string   `hcl:"required_version,optional"`
	Remain             hcl.Body `hcl:",remain"`
}

func (ti *terraformInstaller) get(workDir string) (string, error) {
	versionsFilePath := path.Join(workDir, "versions.tf")
	var constraints string

	// get version constraints from versions.tf file
	if _, err := os.Stat(versionsFilePath); err == nil {
		constraints, err = ti.getConstraintsFromFile(versionsFilePath)
		if err != nil {
			return "", err
		}
	}

	// set default constraints if not set in module file
	if constraints == "" {
		constraints = defaultConstraints
	}

	// get installer based on constraints
	installer, err := ti.getTerraformInstallerFromConstraints(constraints)
	if err != nil {
		return "", err
	}

	// return exec
	return ti.download(installer)
}

func (ti *terraformInstaller) getConstraintsFromFile(versionsFilePath string) (string, error) {
	var versionsFile VersionsFile
	file, err := ioutil.ReadFile(versionsFilePath)
	if err != nil {
		return "", err
	}
	err = hclsimple.Decode("versions.hcl", file, nil, &versionsFile)
	if err != nil {
		return "", err
	}
	return versionsFile.Terraform.VersionConstraints, nil
}

func (ti *terraformInstaller) getTerraformInstallerFromConstraints(constraints string) (*releases.ExactVersion, error) {
	verConstraints, err := version.NewConstraint(constraints)
	if err != nil {
		return nil, err
	}
	versions := &releases.Versions{
		Product:     product.Terraform,
		Constraints: verConstraints,
	}
	matchingVersions, err := versions.List(context.TODO())
	if err != nil {
		return nil, err
	}
	if len(matchingVersions) == 0 {
		return nil, fmt.Errorf("no matching versions for constraint: %v", constraints)
	}

	latest := matchingVersions[len(matchingVersions)-1]
	return latest.(*releases.ExactVersion), nil
}

func (ti *terraformInstaller) download(installer *releases.ExactVersion) (string, error) {
	requiredVer := installer.Version.String()
	execDir := path.Join(tempBinariesFolder, requiredVer)
	if _, ok := ti.versions[requiredVer]; !ok {
		log.Debugln("Installing new terraform version: ", requiredVer)
		createFolder(execDir)
		installer.InstallDir = execDir
		execPath, err := installer.Install(context.TODO())
		if err != nil {
			return "", err
		}
		ti.versions[requiredVer] = execPath

	}
	return ti.versions[requiredVer], nil
}

func createFolder(p string) error {
	return os.MkdirAll(p, os.ModePerm)
}

func main() {
	// set error counter, mutex and waiting group for the goroutines
	var wg sync.WaitGroup
	var errCount int32
	var m sync.Mutex

	// set exit code equal to the number of failures
	defer func() {
		log.Debugln("Total errors: ", errCount)
		os.Exit(int(errCount))
	}()

	// create installer
	installer := &terraformInstaller{versions: make(map[string]string)}

	// create temporary folder for binaries
	log.Debugln("Create temporary folder for binaries: ", tempBinariesFolder)
	if err := createFolder(tempBinariesFolder); err != nil {
		log.Fatal(err)
	}

	// delete temp folder at the end of the execution
	defer func() {
		log.Debugln("Delete binaries temporary folder")
		os.RemoveAll(tempBinariesFolder)
	}()

	// get all changed modules for this commit
	modules, err := findChangedModules()
	if err != nil {
		log.Fatal(err)
	}

	// run over all changed modules
	log.Info("Modules to check: ", strings.Join(modules, ", "))
	for _, module := range modules {
		wg.Add(1)

		go func(module string) {
			defer wg.Done()

			logger := log.WithFields(log.Fields{"module": module})

			// lock `Get` method to avoid multiple downloads of similar versions
			m.Lock()
			execPath, err := installer.get(module)
			m.Unlock()
			if err != nil {
				logger.Error(err)
				atomic.AddInt32(&errCount, 1)
				return
			}

			// configure tfexec client
			tf, err := tfexec.NewTerraform(module, execPath)
			if err != nil {
				logger.Error(err)
				atomic.AddInt32(&errCount, 1)
				return
			}

			// run init before validation (required)
			err = tf.Init(context.TODO(), tfexec.Backend(false))
			if err != nil {
				logger.Warnf("Terraform init error: %v", err)
			}

			// validate terraform module
			validate, err := tf.Validate(context.TODO())
			if err != nil {
				logger.Error(err)
				atomic.AddInt32(&errCount, 1)
				return
			}

			// check if validation failed
			if !validate.Valid {
				atomic.AddInt32(&errCount, 1)
			}

			// print the validation result
			for _, d := range validate.Diagnostics {
				logger := log.WithFields(log.Fields{
					"module":   module,
					"filename": d.Range.Filename,
					"line":     d.Range.Start.Line,
				})
				if d.Severity == tfjson.DiagnosticSeverityError {
					logger.Errorln(d.Detail)
				} else if d.Severity == tfjson.DiagnosticSeverityWarning {
					logger.Warnln(d.Summary)
				}
			}

			// format terraform code
			isFormatted, files, _ := tf.FormatCheck(context.TODO())
			if !isFormatted {
				logger.Error("Unformatted files: ", strings.Join(files, ", "))
				atomic.AddInt32(&errCount, 1)
			}

		}(module)
	}

	// wait for all goroutines to finish
	wg.Wait()
}

func runCommand(name string, args ...string) string {
	log.Infoln("Running command: ", name, args)
	cmd := exec.Command(name, args...)

	output, err := cmd.CombinedOutput()
	resp := string(output)

	if err != nil {
		log.Fatal(resp)
	}

	log.Infoln(output)

	return resp
}

func findChangedModules() ([]string, error) {
	// get pull_requests properties
	dstBranch := os.Getenv("GITHUB_BASE_REF")
	workspace := os.Getenv("GITHUB_WORKSPACE")

	log.Debugln("workspace:", workspace)
	log.Debugln("target-branch:", dstBranch)

	var modules []string
	uniqueMap := make(map[string]struct{})

	// addressing ownership issue https://github.com/actions/checkout/issues/766
	runCommand("git", "config", "--global", "--add", "safe.directory", workspace)

	// use git diff to get all changed files
	output := runCommand("git", "diff", "--name-only", fmt.Sprintf("origin/%v...", dstBranch), "--", workspace)
	log.Debugln("Changed files: ", output)

	for _, line := range strings.Split(output, "\n") {

		// remove empty lines
		if line == "" {
			continue
		}

		// remove non-tf changes
		if !strings.HasSuffix(line, ".tf") {
			continue
		}

		// remove the last element of the path (filename)
		fields := strings.Split(line, "/")
		fields = fields[:len(fields)-1]

		uniqueMap[strings.Join(fields, "/")] = struct{}{}
	}

	// convert the map to a slice and return it
	for module := range uniqueMap {
		modules = append(modules, path.Join(workspace, module))
	}

	return modules, nil
}
