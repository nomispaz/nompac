package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// colors for printout
var Reset = "\033[0m"
var Red = "\033[31m"
var Green = "\033[32m"
var Yellow = "\033[33m"
var Blue = "\033[34m"
var Magenta = "\033[35m"
var Cyan = "\033[36m"
var Gray = "\033[37m"
var White = "\033[97m"

type Args struct {
	snapshot       string
	pacconfig      string
	config         string
	package_groups string
	initiate       string
}

// Define a struct for the Patches part of the JSON
type Patches map[string][]string

// Define a struct for the Packages part of the JSON
type Packages map[string][]string

// PatchConfig represents the structure of the configuration file
type Config struct {
	build_dir     string     `json:"BuildDir"`
	patch_dir     string     `json:"PatchDir"`
	overlay_dir   string     `json:"OverlayDir"`
	local_repo    string     `json:"LocalRepoDir"`
	name          string     `json:"Name"`
	packages      []Packages `json:"Packages"`
	overlays      []string   `json:"Overlays"`
	patches       []Patches  `json:"Patches"`
	packagegroups string     `json:"PackageGroups"`
	pacconfig     string     `json:"Pacconfig"`
	mirrorlist    string     `json:"Mirrorlist"`
	snapshot      string     `json:"Snapshot"`
}

// read current package version from repository
// takes package name and returns version-revision
func get_current_version_from_repo(package_name string) string {

	// URL of the PKGBUILD file in GitLab raw format
	url := fmt.Sprintf("https://gitlab.archlinux.org/archlinux/packaging/packages/%s/-/raw/main/PKGBUILD", package_name)

	// Fetch the PKGBUILD file
	response, err := http.Get(url)

	if err != nil {
		fmt.Printf("Failed to fetch PKGBUILD file: %s\n", package_name)
		return err.Error()
	}
	defer response.Body.Close()

	// Check for statuscode to ensure that the body contains a valid packagebuild
	if response.StatusCode != http.StatusOK {
		fmt.Printf("Failed to fetch PKGBUILD file: %d\n", response.StatusCode)
		return "HTTP-Status-Code: " + string(response.StatusCode)
	}

	// convert io.Reader to []byte
	contents, _ := io.ReadAll(response.Body)

	return get_version_from_pkgbuild(string(contents))
}

// takes the config struct and the name of the package and returns the version-revision of the package
func get_version_from_overlay(config Config, packagename string) string {
	url := filepath.Join(config.overlay_dir, packagename, "PKGBUILD")
	// read file to string
	contents_bytes, err := os.ReadFile(url)

	if err != nil {
		fmt.Printf("Package version of package %s from overlay couldn't be determinded: %s", packagename, err.Error())
		return err.Error()
	}

	return get_version_from_pkgbuild(string(contents_bytes))

}

// Extract version from pkgbuild-file that was given as string in file_contents
func get_version_from_pkgbuild(file_contents string) string {

	pkgver := ""
	pkgrel := ""

	for _, line := range strings.Split(file_contents, "\n") {
		if strings.HasPrefix(line, "pkgver=") {
			pkgver = strings.Split(line, "=")[1]
		}
		if strings.HasPrefix(line, "pkgrel=") {
			pkgrel = strings.Split(line, "=")[1]
		}
	}
	return fmt.Sprintf("%s-%s", pkgver, pkgrel)
}

// fetch the tarball from arch online repository.
// parameters: package_name, package_version, file_path
func get_current_tarball_from_repo(package_name string, package_version string, file_path string) {
	// URL of the tar.gz file in GitLab
	url := fmt.Sprintf("https://gitlab.archlinux.org/archlinux/packaging/packages/%s/-/archive/%s/%s-%s.tar.gz", package_name, package_version, package_name, package_version)

	// Create the file
	out, err := os.Create(file_path)
	if err != nil {
		fmt.Printf("Failed to create file: %v\n", err)
		return
	}
	defer out.Close()

	// Fetch the tar.gz file
	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("Failed to fetch tar.gz file: %v\n", err)
		return
	}
	defer resp.Body.Close()

	// Check if the request was successful
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Failed to fetch tar.gz file: %d\n", resp.StatusCode)
		return
	}

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		fmt.Printf("Failed to write tar.gz file: %v\n", err)
		return
	}

	fmt.Printf("Successfully downloaded %s-%s.tar.gz\n", package_name, package_version)
}

// extract the downloaded tarball
func extract_tgz(filename, output_path string) error {
	// Open the tar.gz file
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open tar.gz file: %w", err)
	}
	defer file.Close()

	// Create a gzip reader
	gzr, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// Create a tar reader
	tr := tar.NewReader(gzr)

	// Extract the tarball
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return fmt.Errorf("failed to read tarball: %w", err)
		}

		// Determine the proper file path
		target := filepath.Join(output_path, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			// Create directory
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case tar.TypeReg:
			// Create file
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to create file: %w", err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("failed to copy file: %w", err)
			}
			f.Close()
		}
	}
	return nil
}

// takes PKGBUILD file and patchname and adds the patch to the file
func modify_pkgbuild(file string, patch string, package_name string) {

	block_state := "none"
	prepare_block_exists := false
	modified_content := ""

	contents, err := os.ReadFile(file)

	if err != nil {
		fmt.Printf("Couldn't open file: %s", err.Error())
		return
	}

	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "source") {
			block_state = "source"
		}
		if strings.HasPrefix(strings.TrimLeft(line, " "), "prepare") {
			prepare_block_exists = true
			block_state = "prepare"
		}
		if block_state == "source" && strings.HasSuffix(strings.TrimRight(line, " "), ")") {
			modified_content += fmt.Sprintf("    \"%s\"\n", patch)
			block_state = "none"
		}
		if block_state == "prepare" && strings.HasSuffix(line, "}") {
			modified_content += fmt.Sprintf("    patch -Np1 -i \"${srcdir}/%s\"\n", patch)
			block_state = "none"
		}

		modified_content += line + "\n"
	}

	// if no prepare block exists in the PKGBUILD, append the block with the patch command
	if !prepare_block_exists {
		modified_content += fmt.Sprintf("\nprepare() {\n    cd %s-\"${pkgver}\"\n    patch -Np1 -i \"${srcdir}/%s\"\n}\n", package_name, patch)
	}

	err = os.WriteFile(file, []byte(modified_content), 0644)
	if err != nil {
		fmt.Printf("Couldn't write PKGBUILD-file: %s\n", file)
	}
}

// funtion takes the configuration, a vector of packages, the package name and the package version
// patches should be applied and the path to the PKBBUILD file.
// Then the function modifies the PKGBUILD file.
func applyPatches(config Config, patches []string, packagename string, packageversion string) {
	for _, patch := range patches {
		pkg_build_dir := filepath.Join(config.build_dir, fmt.Sprintf("%s-%s", packagename, packageversion))
		copyFile(
			filepath.Join(config.patch_dir, packagename, patch),
			filepath.Join(pkg_build_dir, patch),
		)
		modify_pkgbuild(filepath.Join(pkg_build_dir, "PKGBUILD"), patch, packagename)
	}
}

func buildPackage(pkg_build_dir string) {
	commands := "pushd " + pkg_build_dir +
		"; updpkgsums" +
		"; makepkg -cCsr --skippgpcheck" +
		"; popd"
	execCmd(commands)
}

// takes config struct and packagename and updates the repository so that a build package is
// copied to the local repository directory and added to the directory
func update_repository(config Config, local_repo_dir string, packagename string) {
	files, _ := filepath.Glob(filepath.Join(config.build_dir, "src", packagename, "**", "*.pkg.tar.zst"))

	for _, entry_result := range files {
		copyFile(
			entry_result,
			filepath.Join(local_repo_dir, filepath.Base(entry_result)),
		)
		command := fmt.Sprintf(
			"repo-add %s/nompaz.db.tar.zst %s/%s",
			local_repo_dir,
			local_repo_dir,
			filepath.Base(entry_result),
		)
		execCmd(command)
	}
}

// cleans the build directory
func cleanup(config Config) {
	command := fmt.Sprintf("rm -r %s/src", config.build_dir)
	execCmd(command)
}

// takes the package name and returns version-revision of the installed package
func get_installed_version(packagename string) string {
	// Construct the command
	command := fmt.Sprintf("pacman -Q | grep \"\\<%s\\>\" | cut -d' ' -f 2", packagename)
	cmd := exec.Command("bash", "-c", command)

	// Run the command and capture the output
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		fmt.Println("Error while reading package version of %s: %e", packagename, err)
	}

	// Save the output to a variable
	output := out.String()
	if len(output) > 0 {
		return output
	} else {
		fmt.Println("No version found for package %s", packagename)
		return ""
	}
}

// Replace a row in filename containing the pattern with replacement.
// Set append_if_not_exist to 1 if the replacement should be added to the end of the file if the pattern wasn't found.
// The pattern needs to be given as regex
// be mindfull of special characters in the pattern, e.g.
// $$      Match single dollar sign.

func modifyFile(filename string, pattern string, replacement string, append_if_not_exist bool) {

	contents_bytes, err := os.ReadFile(filename)
	file_content := string(contents_bytes)
	if err != nil {
		fmt.Println("Error opening file %s: %e", filename, err)
		return
	}

	re, _ := regexp.Compile(pattern)
	pattern_found, _ := regexp.MatchString(pattern, file_content)

	if pattern_found {
		// replace the pattern
		file_content = re.ReplaceAllString(file_content, replacement)
	}

	if !pattern_found && append_if_not_exist {
		// the content didn't exist --> it couldn't be replaced and needs to be appended to the file
		file_content = file_content + "\n" + replacement
	}

	// Write the modified lines back to the file
	file, err := os.Create(filename)
	if err != nil {
		fmt.Println("Error opening file %s for writing: %e", filename, err)
		return
	}
	defer file.Close()

	_, err = file.WriteString(file_content)
	if err != nil {
		fmt.Println("Error writing file %s: %e", filename, err)
		file.Close()
	}
}

// takes the path to the config file, parses the json files and returns a config struct
func parse_config(file_path string, args Args) Config {
	// read json file to string
	contents, err := os.ReadFile(file_path)

	if err != nil {
		fmt.Printf(Red + "Failed to open config file." + Reset)
		panic(err)
	}

	// initialize the config map
	var configs Config

	// Unmarshal the JSON into the Config struct
	err = json.Unmarshal(contents, &configs)

	if err != nil {
		fmt.Println(Red+"Error in the config file: %e"+Reset, err)
		panic(err)
	}

	// use pacconfig from args if available
	if args.pacconfig != "none" {
		configs.pacconfig = args.pacconfig
	}

	configs.pacconfig = resolve_home(configs.pacconfig)

	// if overlay-dir starts with ~ or $HOME, parse the directory
	configs.overlay_dir = resolve_home(configs.overlay_dir)

	// if patch-dir starts with ~ or $HOME, parse the directory
	configs.patch_dir = resolve_home(configs.patch_dir)

	// if overlay-dir starts with ~ or $HOME, parse the directory
	configs.mirrorlist = resolve_home(configs.mirrorlist)

	if strings.HasSuffix(strings.TrimRight(configs.local_repo, " "), ".db.tar.zst") {
		configs.local_repo = resolve_home(configs.local_repo)
		// does the file exist?
		_, err = os.Stat(configs.local_repo)
		if err != nil {
			//initiate, if anything other the no or n is defined
			if args.initiate != "no" && args.initiate != "n" {
				fmt.Println("Repository file doesn't exist. It will be created.")
				initiate_repo(configs)
				configs.local_repo = filepath.Base(configs.local_repo)

			} else {
				configs.local_repo = "none"
				fmt.Println(Red + "No db.tar.zst-file for local repository specified -> no local builds are possible. To create the file, restart with -i yes" + Reset)
			}
		} else {
			// repo file exists --> get path to repo
			configs.local_repo = filepath.Base(configs.local_repo)
		}
	} else {
		configs.local_repo = "none"
		fmt.Println(Red + "No db.tar.zst-file for local repository specified -> no local builds are possible" + Reset)
	}
	return configs
}

func resolve_home(path string) string {
	home_dir, _ := os.UserHomeDir()

	if strings.HasPrefix(strings.TrimLeft(path, " "), "~") {
		path = strings.ReplaceAll(path, "~", home_dir)
	}
	if strings.HasPrefix(strings.TrimLeft(path, " "), "$HOME") {
		path = strings.ReplaceAll(path, "$HOME", home_dir)
	}
	return path
}

// initiate nompac.
// Takes config struct
// Creates local repo according to the defined local_repo config option
func initiate_repo(config Config) {
	local_repo_file := filepath.Base(config.local_repo)
	execCmd("repo-add " + local_repo_file)
}

// initiate nompac.
// Takes config struct
// Updates pacman.conf with configured mirrorlist and adds local repo
func initiate_pacmanconf(config Config) {
	// change mirrorlist to the one configured
	modifyFile(config.pacconfig, "Include.*mirrorlist", "Include = "+config.mirrorlist, false)

	// add local repository
	if config.local_repo != "none" {
		contents_bytes, err := os.ReadFile(config.pacconfig)
		if err != nil {
			fmt.Println("Couldn't read pacconfig %s: %e", config.pacconfig, err)
		}
		file_contents := string(contents_bytes)
		var modified_content string
		already_inserted := false
		for _, line := range strings.Split(file_contents, "\n") {
			if !already_inserted &&
				(strings.HasSuffix(line, "[core-testing]") ||
					strings.HasSuffix(line, "[core]") ||
					strings.HasSuffix(line, "[extra-testing]") ||
					strings.HasSuffix(line, "[extra]") ||
					strings.HasSuffix(line, "[multilib]")) {
				modified_content +=
					"[nomispaz]\n" +
						"Siglevel = Optional TrustAll\n" +
						"Server = file://%s" + config.local_repo + "\n\n" +
						line + "\n"

				already_inserted = true
			} else {
				modified_content += line + "\n"
			}
		}
		err = os.WriteFile(config.pacconfig, []byte(modified_content), 0644)
		if err != nil {
			fmt.Printf("Couldn't write pacconfig-file %s: %e\n", config.pacconfig, err)
		}
	}
}

func copyFile(src string, dst string) error {
	// Open the source file
	sourceFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer sourceFile.Close()

	// Create the destination file
	destinationFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destinationFile.Close()

	// Copy the file contents from source to destination
	_, err = io.Copy(destinationFile, sourceFile)
	if err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	// Sync to flush writes to stable storage
	err = destinationFile.Sync()
	if err != nil {
		return fmt.Errorf("failed to sync destination file: %w", err)
	}

	return nil
}

// TODO rewrite with async
func execCmd(command string) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run() // add error checking
}

func parse_args() Args {
	var args Args

	args.snapshot = *flag.String("snapshot", "none", "Defines the date of the Arch-repository snapshot that should be used. Always enter in the format YYYY_MM_DD. If no date is entered, no update will be performed.")

	args.pacconfig = *flag.String("pacconfig", "none", "Provides the pacconfig-file")

	args.config = *flag.String("config", "~/.config/nompac_rs/configs/config.json", "Config file for nompac")

	args.package_groups = *flag.String("packagegroups", "none", "Define package groups that should be used seperated by a comma ','")

	args.initiate = *flag.String("initiate", "no", "Set to yes if the pacconfig file and the local repository file should be generated in this run.")

	flag.Parse()

	return args
}

func collect_package_lists(configs Config, args Args) ([]string, []string) {
	// collect packages that are installed explicitely
	var package_list_installed []string

	command := "pacman -Qe | cut -d' ' -f 1"
	cmd, err := exec.Command("bash", "-c", command).Output()

	if err != nil {
		fmt.Println(Red + "Couldn't get list of installed packages: err" + Reset)
	}

	package_list_installed = strings.Fields(string(cmd))

	// use package group from args if available, otherwise from config-file
	var package_groups []string

	if args.package_groups != "none" {
		package_groups = strings.Split(args.package_groups, ",")
	} else {
		package_groups = strings.Split(configs.packagegroups, ",")
	}

	// create package list of packages that should be installed explicitely
	// slice to include all packages to be installed explicitely
	var package_list []string

	for group, packagelist := range configs.packages[0] {
		if strings.Contains(package_groups[0], group) || strings.Contains(package_groups[0], "all") {
			for _, pkg := range packagelist {
				package_list = append(package_list, strings.ToLower(pkg))
			}
		}
	}

	// sort the resulting slice of packages
	sort.Strings(package_list)

	// search for packages that are installed but not in package_list
	// for this, iterate over list and remove the package already read from the vector
	var packages_to_remove []string
	for _, pkg := range package_list_installed {
		for entry := range package_list {
			if !strings.Contains(package_list[entry], pkg) {
				packages_to_remove = append(packages_to_remove, pkg)
			}

		}
	}

	// search for packages that are in the config file but not explicitely installed
	var packages_to_install []string
	for _, pkg := range package_list {
		for entry := range package_list_installed {
			if !strings.Contains(package_list_installed[entry], pkg) {
				packages_to_install = append(packages_to_install, pkg)
			}

		}
	}

	return packages_to_remove, packages_to_install
}

// TODO: rewrite.
// switch some tasks to readConfig
func main() {
	// define and read command line arguments
	args := parse_args()

	// read JSON configuration file for nompac
	configs := parse_config(resolve_home(args.config), args)

	// initiate pacman.conf if required
	if args.initiate != "no" && args.initiate != "n" {
		initiate_pacmanconf(configs)
	}

	var date []string

	// if a snapshot was defined in the arguments, replace the one from the config file
	if args.snapshot == "none" {
		date = strings.Split(configs.snapshot, "_")
	} else {
		date = strings.Split(args.snapshot, "_")
	}

	fmt.Println(Blue + "Used settings:" + Reset)
	fmt.Println("Local build directory: " + configs.build_dir)
	fmt.Println("Local repository: " + configs.local_repo)
	fmt.Println("Patch directory: " + configs.patch_dir)
	fmt.Println("Overlay directory: " + configs.overlay_dir)
	fmt.Println("pacman.conf location: " + configs.pacconfig)

	os.MkdirAll(configs.build_dir, os.ModeDir)

	packages_to_remove, packages_to_install := collect_package_lists(configs, args)

	fmt.Println(Blue + "Building patched upstream-packages" + Reset)

	//building custom packages and overlays
	if configs.local_repo != "none" {
		fmt.Println(Blue + "\nBuilding patched upstream-packages" + Reset)

		// create necessary directories
		// build directory
		os.MkdirAll(filepath.Join(configs.build_dir, "src"), os.ModeDir)

		// apply patches, build new package and update local repository
		for _, package_list := range configs.patches[0] {
			for _, pkg := range package_list {
				package_version_repo := get_current_version_from_repo(pkg)
				package_version_installed := get_installed_version(pkg)
				//only procede if the package was updated upstream
				if strings.Trim(package_version_installed, " ") != strings.Trim(package_version_repo, " ") {
					get_current_tarball_from_repo(pkg, package_version_repo, fmt.Sprintf("%s/%s-%s.tar.gz", configs.build_dir, pkg, package_version_repo))

					fmt.Println("%s/%s-%s.tar.gz", configs.build_dir, pkg, package_version_repo)

					extract_tgz(fmt.Sprintf("%s/%s-%s.tar.gz", configs.build_dir, pkg, package_version_repo), filepath.Join(configs.build_dir, "src"))

					applyPatches(configs, configs.patches[0][pkg], pkg, package_version_repo)

					buildPackage(fmt.Sprintf("%s/src/%s-%s/", configs.build_dir, pkg, package_version_repo))

					update_repository(configs, configs.local_repo, pkg)
				} else {
					fmt.Println(Green+"Package %s already up to date."+Reset, pkg)
				}
			}
		}
		fmt.Println(Blue + "\nBuilding packages from overlays" + Reset)
		for _, pkg := range configs.overlays {
			package_version_overlay := get_version_from_overlay(configs, pkg)
			package_version_installed := get_installed_version(pkg)

			if strings.Trim(package_version_installed, " ") != strings.Trim(package_version_overlay, " ") {
				// copy necessary files from overlay to build directory
		//TODO continue from here

			}
		}
	}







	// ab hier alter Code

	fmt.Println(Blue + "Building packages from overlays" + Reset)
	for _, packageName := range config.overlays {
		// read package version from local package overlay
		packageVersion := get_version_from_overlay(config, packageName)
		fileName := packageName + "-" + packageVersion
		// read version of installed package
		installedPkgVersion := get_installed_version(packageName)
		// only build package if the package version in the PKGBUILD is different thatn the installed version.
		if strings.Compare(strings.Trim(installedPkgVersion, "\n"), strings.Trim(packageVersion, "\n")) != 0 {
			err := os.CopyFS(config.build_dir+"/src/"+fileName, os.DirFS(filepath.Join(config.overlay_dir, packageName)))

			if err != nil {
				panic(err)
			}

			buildPackage(config, fileName)
			updateRepository(config, packageName, fileName)
			cleanup(config)
		} else {
			fmt.Println("No new version of package " + packageName + " available --> no rebuild")
		}

	}

	if *update != "no" {
		fmt.Println(Blue + "Updating mirrorlist and system" + Reset)

		modifyFile("/home/simonheise/git_repos/nompac/configs/mirrorlist", "archive.archlinux.org", "Server = https://archive.archlinux.org/repos/"+snapshot_year+"/"+snapshot_month+"/"+snapshot_day+"/$repo/os/$arch")

		fmt.Println(Blue + "Rebuilding the system." + Reset)
		fmt.Print(Red + "Removing the following packages: " + Reset)
		command := "sudo pacman -Rsn"
		printDelete := ""
		for i := range packagesToDelete {
			command += " " + packagesToDelete[i]
			printDelete += packagesToDelete[i]
		}
		fmt.Println(printDelete)
		execCmd(command)

		printInstall := ""
		fmt.Print(Red + "Installing the following packages and updating system: " + Reset)
		command = "sudo pacman -Syu"
		for i := range packagesToInstall {
			command += " " + packagesToInstall[i]
			printInstall += packagesToInstall[i]
		}
		command += " --config " + *pacconfig +
			"; sudo DIFFPROG='nvim -d' pacdiff"
		fmt.Println(printInstall)
		execCmd(command)

	}

}
