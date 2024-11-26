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
	Build_dir     string     `json:"build_dir"`
	Patch_dir     string     `json:"patch_dir"`
	Overlay_dir   string     `json:"overlay_dir"`
	Local_repo    string     `json:"local_repo"`
	Name          string     `json:"name"`
	Packages      []Packages `json:"packages"`
	Overlays      []string   `json:"overlays"`
	Patches       []Patches  `json:"patches"`
	Packagegroups string     `json:"packagegroups"`
	Pacconfig     string     `json:"pacconfig"`
	Mirrorlist    string     `json:"mirrorlist"`
	Snapshot      string     `json:"snapshot"`
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
	url := filepath.Join(config.Overlay_dir, packagename, "PKGBUILD")
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
			if err := os.MkdirAll(target, os.FileMode(0777)); err != nil {
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
	fmt.Println("Successfully extracted " + filename)
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
	} else {
		fmt.Println("Successfully applied patch " + patch)

	}
}

// funtion takes the configuration, a vector of packages, the package name and the package version
// patches should be applied and the path to the PKBBUILD file.
// Then the function modifies the PKGBUILD file.
func applyPatches(config Config, patches []string, packagename string, packageversion string) {
	for _, patch := range patches {
		fmt.Println("Applying patch " + patch)
		pkg_build_dir := filepath.Join(config.Build_dir, "src", fmt.Sprintf("%s-%s", packagename, packageversion))
		copyFile(
			filepath.Join(config.Patch_dir, packagename, patch),
			filepath.Join(pkg_build_dir, patch),
		)
		modify_pkgbuild(filepath.Join(pkg_build_dir, "PKGBUILD"), patch, packagename)
	}
}

func buildPackage(pkg_build_dir string) {
	fmt.Println("Building package in: ", pkg_build_dir)
	commands := "pushd " + pkg_build_dir +
		"; updpkgsums" +
		"; makepkg -cCsr --skippgpcheck" +
		"; popd"
	execCmd(commands)
}

// takes config struct and packagename and updates the repository so that a build package is
// copied to the local repository directory and added to the directory
func update_repository(config Config, local_repo_dir string, packagename string) {
	files, _ := filepath.Glob(filepath.Join(config.Build_dir, "src", packagename, "**", "*.pkg.tar.zst"))

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
	command := fmt.Sprintf("rm -r %s/src", config.Build_dir)
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
		fmt.Println("Error while reading package version of ", packagename, err)
	}

	// Save the output to a variable
	output := out.String()
	if len(output) > 0 {
		return output
	} else {
		fmt.Println("No version found for package ", packagename)
		return ""
	}
}

// Replace a row in filename containing the pattern with replacement.
// Set append_if_not_exist to 1 if the replacement should be added to the end of the file if the pattern wasn't found.
// The pattern needs to be given as regex
// be mindfull of special characters in the pattern, e.g.
// $$      Match single dollar sign.

func modify_file(filename string, pattern string, replacement string, append_if_not_exist bool) {

	contents_bytes, err := os.ReadFile(filename)

	if err != nil {
		fmt.Println("Error opening file ", filename, err)
		return
	}

	file_content := string(contents_bytes)

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
		fmt.Println("Error opening file  ", filename, " for writing: ", err)
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
		configs.Pacconfig = args.pacconfig
	}

	configs.Pacconfig = resolve_home(configs.Pacconfig)

	// if overlay-dir starts with ~ or $HOME, parse the directory
	configs.Overlay_dir = resolve_home(configs.Overlay_dir)

	// if patch-dir starts with ~ or $HOME, parse the directory
	configs.Patch_dir = resolve_home(configs.Patch_dir)

	// if overlay-dir starts with ~ or $HOME, parse the directory
	configs.Mirrorlist = resolve_home(configs.Mirrorlist)

	if strings.HasSuffix(strings.TrimRight(configs.Local_repo, " "), ".db.tar.zst") {
		configs.Local_repo = resolve_home(configs.Local_repo)
		// does the file exist?
		_, err = os.Stat(configs.Local_repo)
		if err != nil {
			//initiate, if anything other the no or n is defined
			if args.initiate != "no" && args.initiate != "n" {
				fmt.Println("Repository file doesn't exist. It will be created.")
				initiate_repo(configs)
				configs.Local_repo = filepath.Base(configs.Local_repo)

			} else {
				configs.Local_repo = "none"
				fmt.Println(Red + "No db.tar.zst-file for local repository specified -> no local builds are possible. To create the file, restart with -i yes" + Reset)
			}
		} else {
			// repo file exists --> get path to repo
			configs.Local_repo = filepath.Base(configs.Local_repo)
		}
	} else {
		configs.Local_repo = "none"
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
	local_repo_file := filepath.Base(config.Local_repo)
	execCmd("repo-add " + local_repo_file)
}

// initiate nompac.
// Takes config struct
// Updates pacman.conf with configured mirrorlist and adds local repo
func initiate_pacmanconf(config Config) {
	// change mirrorlist to the one configured
	modify_file(config.Pacconfig, "Include.*mirrorlist", "Include = "+config.Mirrorlist, false)

	// add local repository
	if config.Local_repo != "none" {
		contents_bytes, err := os.ReadFile(config.Pacconfig)
		if err != nil {
			fmt.Println("Couldn't read pacconfig %s: %e", config.Pacconfig, err)
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
						"Server = file://%s" + config.Local_repo + "\n\n" +
						line + "\n"

				already_inserted = true
			} else {
				modified_content += line + "\n"
			}
		}
		err = os.WriteFile(config.Pacconfig, []byte(modified_content), 0644)
		if err != nil {
			fmt.Printf("Couldn't write pacconfig-file %s: %e\n", config.Pacconfig, err)
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

	snapshot := flag.String("snapshot", "none", "Defines the date of the Arch-repository snapshot that should be used. Always enter in the format YYYY_MM_DD. If no date is entered, no update will be performed.")

	pacconfig := flag.String("pacconfig", "none", "Provides the pacconfig-file")

	config := flag.String("config", "~/.config/nompac/configs/config.json", "Config file for nompac")

	package_groups := flag.String("packagegroups", "none", "Define package groups that should be used seperated by a comma ','")

	initiate := flag.String("initiate", "no", "Set to yes if the pacconfig file and the local repository file should be generated in this run.")

	flag.Parse()
	
	args := Args{
		snapshot: *snapshot,
		pacconfig: *pacconfig,
		config: *config,
		package_groups: *package_groups,
		initiate: *initiate,
	}

	return args
}

func contains(slice []string, str string) bool {
	for _, item := range slice {
		if item == str {
			return true
		}
	}
	return false
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
		package_groups = strings.Split(configs.Packagegroups, ",")
	}

	// create package list of packages that should be installed explicitely
	// slice to include all packages to be installed explicitely
	var package_list []string

	for group, packagelist := range configs.Packages[0] {
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
		if !contains(package_list, pkg) {
			packages_to_remove = append(packages_to_remove, pkg)
		}
	}

	// search for packages that are in the config file but not explicitely installed
	var packages_to_install []string
	for _, pkg := range package_list {
		if !contains(package_list_installed, pkg) {
			packages_to_install = append(packages_to_install, pkg)
		}

	}

	return packages_to_remove, packages_to_install
}

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
	if args.snapshot != "none" {
		date = strings.Split(args.snapshot, "_")
	} else {
		date = strings.Split(configs.Snapshot, "_")
	}

	fmt.Println(Blue + "Used settings:" + Reset)
	fmt.Println("Local build directory: " + configs.Build_dir)
	fmt.Println("Local repository: " + configs.Local_repo)
	fmt.Println("Patch directory: " + configs.Patch_dir)
	fmt.Println("Overlay directory: " + configs.Overlay_dir)
	fmt.Println("pacman.conf location: " + configs.Pacconfig)

	os.MkdirAll(configs.Build_dir, os.FileMode(0777))

	packages_to_remove, packages_to_install := collect_package_lists(configs, args)

	//building custom packages and overlays
	if configs.Local_repo != "none" {
		fmt.Println(Blue + "\nBuilding patched upstream-packages" + Reset)

		// create necessary directories
		// build directory
		os.MkdirAll(filepath.Join(configs.Build_dir, "src"), os.FileMode(0777))

		// apply patches, build new package and update local repository
		for pkg, patches := range configs.Patches[0] {
			package_version_repo := get_current_version_from_repo(pkg)
			package_version_installed := get_installed_version(pkg)

			//only procede if the package was updated upstream
			if strings.TrimSpace(package_version_installed) != strings.TrimSpace(package_version_repo) {
				get_current_tarball_from_repo(pkg, package_version_repo, fmt.Sprintf("%s/%s-%s.tar.gz", configs.Build_dir, pkg, package_version_repo))

				fmt.Printf("%s/%s-%s.tar.gz\n", configs.Build_dir, pkg, package_version_repo)

				extract_tgz(fmt.Sprintf("%s/%s-%s.tar.gz", configs.Build_dir, pkg, package_version_repo), filepath.Join(configs.Build_dir, "src"))

				applyPatches(configs, patches, pkg, package_version_repo)

				buildPackage(fmt.Sprintf("%s/src/%s-%s/", configs.Build_dir, pkg, package_version_repo))

				update_repository(configs, configs.Local_repo, pkg)
			} else {
				fmt.Println(Green + fmt.Sprintf("Package %s already up to date."+Reset, pkg))
			}
		}
	}
	fmt.Println(Blue + "\nBuilding packages from overlays" + Reset)
	for _, pkg := range configs.Overlays {
		package_version_overlay := get_version_from_overlay(configs, pkg)
		package_version_installed := get_installed_version(pkg)

		if strings.TrimSpace(package_version_installed) != strings.TrimSpace(package_version_overlay) {
			// copy necessary files from overlay to build directory
			filepath.WalkDir(filepath.Join(configs.Overlay_dir, pkg), func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				// if entry is a file,continue
				if !entry.IsDir() {
					// create directory if it is missing
					os.MkdirAll(filepath.Join(configs.Build_dir, "src", pkg), os.FileMode(0777))
					copyFile(path, filepath.Join(configs.Build_dir, "src", pkg, entry.Name()))
				}
				fmt.Println(configs.Build_dir + "/src" + pkg)
				return nil
			})

			// build the package
			buildPackage(filepath.Join(configs.Build_dir, "src", pkg))
			update_repository(configs, configs.Local_repo, pkg)
			cleanup(configs)
		} else {
			fmt.Println(Green + "Package " + pkg + " already up to date" + Reset)
		}
	}

	// perform system update
	if date[0] != "none" {
		// update snapshot that will be used for the update
		modify_file(configs.Mirrorlist, ".*archive.archlinux.org.*", fmt.Sprintf("Server = https://archive.archlinux.org/repos/%s/%s/%s/$$repo/os/$$arch", date[0], date[1], date[2]), true)

		// only perform if packages have to be removed
		if len(packages_to_remove) > 0 {
			fmt.Println(Red + "Removing the following packages since they don't exist in the config file:" + Reset)
			var package_list string
			for _, pkg := range packages_to_remove {
				package_list += " " + pkg
			}
			// TODO: change to async
			fmt.Println(package_list)
			execCmd("sudo pacman -Rsc " + package_list)

		}

		// only perform if packages have to be installed
		if len(packages_to_install) > 0 {
			fmt.Println(Blue + "Installing the following packages and starting update:" + Reset)
			var package_list string
			for _, pkg := range packages_to_install {
				package_list += " " + pkg
			}
			//TODO: change to async
			fmt.Println(package_list)
			execCmd("sudo pacman -Syu --config " + configs.Pacconfig + " " + package_list)

			// after running the update, check for changed config files
			//TODO: run sudo DIFFPROG='nvim -d' pacdiff interactively
		} else {
			fmt.Println(Blue + "Starting system update.\n" + Reset)
			//TODO: change to async
			execCmd("sudo pacman -Syu --config " + configs.Pacconfig)
			// after running the update, check for changed config files
			//TODO: run sudo DIFFPROG='nvim -d' pacdiff interactively

		}
	}
}
