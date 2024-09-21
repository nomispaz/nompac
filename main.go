package main

import (
	"archive/tar"
	"bufio"
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
	"sort"
	"time"

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

// Define a struct for the Patches part of the JSON
type Patches map[string][]string

// Define a struct for the Packages part of the JSON
type Packages map[string][]string

// slice to include all packages to be installed explicitely
var packageList []string

// PatchConfig represents the structure of the configuration file
type Config struct {
	BuildDir string                      `json:"BuildDir"`
	PatchDir string                      `json:"PatchDir"`
	OverlayDir string                      `json:"OverlayDir"`
	LocalRepoDir string		     `json:"LocalRepoDir"`
	Name     string                      `json:"Name"`
	Packages []Packages                    `json:"Packages"`
	Overlays []string                    `json:"Overlays"`
	Patches  []Patches                     `json:"Patches"`
}

func read_config(filepath string) (Config) {
	// read json file to string
	contents, err := os.ReadFile(filepath)

	if err != nil {
		fmt.Printf(Red + "Failed to open config file." + Reset)
		panic(err)
	}

	// initialize the config map
	var config Config

	// Unmarshal the JSON into the Config struct
	err = json.Unmarshal(contents, &config)
    	if err != nil {
        	fmt.Println(Red + "Error unmarshalling JSON." + Reset)
		panic(err)		
    	}

	return config

}

// read current package version from repository 
// takes package name and returns version-revision
func get_current_version_from_repo(package_name string) string {

	// URL of the PKGBUILD file in GitLab raw format
	url := fmt.Sprintf("https://gitlab.archlinux.org/archlinux/packaging/packages/%s/-/raw/main/PKGBUILD", package_name)

	// Fetch the PKGBUILD file
	response, err := http.Get(url)
	if err != nil {
		fmt.Printf("Failed to fetch PKGBUILD file: %v\n", err)
		return err.Error()
	}
	defer response.Body.Close()

	// Check if the request was successful
	if response.StatusCode != http.StatusOK {
		fmt.Printf("Failed to fetch PKGBUILD file: %d\n", response.StatusCode)
		return "HTTP-Status-Code: " + string(response.StatusCode)
	}

	return getVersionfromPKGBUILD(response.Body, package_name)
}

func get_current_version_from_overlay(config Config, package_name string) string {
	url := filepath.Join(config.OverlayDir, package_name, "PKGBUILD")
	// read file to string
	file, err := os.Open(url)
	if err != nil {
        	panic(err)
    	}
    	defer file.Close()

	//contents, _ := os.ReadFile(url)
	//for line := range contents {
	//	fmt.Sprintln(line)
	//}


	return getVersionfromPKGBUILD(file, package_name)

}

func getVersionfromPKGBUILD(file io.Reader, packageName string) string {
	pkgver := ""
	pkgrel := ""

	// Read the response body line by line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if pkgver != "" && pkgrel != "" {
			fmt.Printf("Package version of %s\n", packageName+": "+pkgver+"-"+pkgrel)
			return pkgver+"-"+pkgrel
		} else {
			line := scanner.Text()
			if strings.HasPrefix(line, "pkgver=") {
				// Extract the version
				pkgver = strings.Split(line, "=")[1]
							}
			if strings.HasPrefix(line, "pkgrel=") {
				// Extract the relation
				pkgrel = strings.Split(line, "=")[1]
			}
		}
	}
	
	if err := scanner.Err(); err != nil {
		fmt.Printf("Error reading response body: %v\n", err)
	}
	return "0"
}

// download the tarball of the current package from gitlab
func getCurrentTarballFromRepo(packageName string, packageVersion string, filePath string) {
	// URL of the tar.gz file in GitLab

	url := fmt.Sprintf("https://gitlab.archlinux.org/archlinux/packaging/packages/%s/-/archive/%s/%s.tar.gz", packageName, packageVersion, packageName+"-"+packageVersion)

	// Create the file
	out, err := os.Create(filePath)
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

	fmt.Printf("Successfully downloaded %s.tar.gz\n", packageName+"-"+packageVersion)
}

// extract the downloaded tarball
func extractTarGz(filePath, destDir string) error {
	// Open the tar.gz file
	file, err := os.Open(filePath)
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
		target := filepath.Join(destDir, header.Name)
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

func applyPatches(config Config, patches []string, packageName string, fileName string)  {
	for _, p := range patches {
		fmt.Printf("Patch: %s\n", p)
		copyFile(config.PatchDir+"/"+packageName+"/"+p, filepath.Join(config.BuildDir,"src",fileName,p))
		modifyPKGBUILD(filepath.Join(config.BuildDir,"src",fileName,"PKGBUILD"), p)
	}
}

// modify the PKBUILD file and add patches to the prepare and sources-block
func modifyPKGBUILD(filePath string, patch string) error {
	// Open the PKGBUILD file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open PKGBUILD file: %w", err)
	}
	defer file.Close()

	// Read the PKGBUILD file
	scanner := bufio.NewScanner(file)
	var buf bytes.Buffer
	prepareBlockExists := false
	inPrepareBlock := false
	inSourceBlock := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "source=(") {
			inSourceBlock = true
		}
		if inSourceBlock && line ==")" {
			buf.WriteString("    " + "\"" + patch + "\"" + "\n")
			inSourceBlock = false
		}
		if strings.HasPrefix(line, "prepare() {") {
			prepareBlockExists = true
			inPrepareBlock = true
		}
		if inPrepareBlock && line == "}" {
			buf.WriteString("    patch -Np1 -i \"${srcdir}/" + patch + "\"\n")
			inPrepareBlock = false
		}
		buf.WriteString(line + "\n")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading PKGBUILD file: %w", err)
	}

	// Modify the PKGBUILD content
	modifiedPKGBUILD := buf.String()
	if !prepareBlockExists {
		modifiedPKGBUILD += fmt.Sprintf("\nprepare() {\n    cd wlroots-\"${pkgver}\"\n    patch -Np1 -i \"${srcdir}/%s\"\n}\n", patch)
	}

	// Save the modified PKGBUILD file
	err = os.WriteFile(filePath, []byte(modifiedPKGBUILD), 0644)
	if err != nil {
		return fmt.Errorf("failed to write modified PKGBUILD file: %w", err)
	}

	return nil
}

func buildPackage(config Config, fileName string)  {
	command := "pushd " + filepath.Join(config.BuildDir,"src",fileName) + 
	"; updpkgsums" +
	"; makepkg -cCsr --skippgpcheck" +
	"; popd"
	fmt.Println(command)
	execCmd(command)
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

func updateRepository(config Config, packageName string, fileName string)  {

	// Walk through the source directory
	err := filepath.Walk(filepath.Join(config.BuildDir,"src", fileName), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Check if the file ends with .xyz
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".pkg.tar.zst") {
			// Construct the destination path
			fmt.Printf("Copying %s to %s\n", path, filepath.Join(config.LocalRepoDir, info.Name()))

			// Copy the file
			err := copyFile(path, filepath.Join(config.LocalRepoDir, info.Name()))
			if err != nil {
				fmt.Println("Error copying file:", err)
				return err
			} else {
				// update repository db
				command := "repo-add " + config.LocalRepoDir + "/nomispaz.db.tar.zst" + " " +config.LocalRepoDir + "/" + info.Name()
				execCmd(command)			}
		}

		return nil
	})
	
	if err != nil {
		panic(err)
	}
}

func cleanup(config Config)  {
	command := "rm -r " + config.BuildDir+"/src"
	fmt.Println(command)
	execCmd(command)
}

func readInstalledPkgVersion(packageName string) string {
	// Construct the command
	command := "pacman -Q | grep " + packageName + " | cut -d' ' -f 2"
	cmd := exec.Command("bash", "-c", command)

	// Run the command and capture the output
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		panic(err)
	}

	// Save the output to a variable
	output := out.String()
	return output
}

func modifyFile(filePath string, searchString string, replaceString string)  {

	// Open the file for reading
	file, err := os.Open(filePath)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return
	}
	defer file.Close()

	// Read the file into a slice of lines
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Println("Error reading file:", err)
		return
	}

	// Find and modify the line containing the string
	found := false
	for i, line := range lines {
		if strings.Contains(line, searchString) {
			lines[i] = replaceString
			found = true
			break
		}
	}

	if !found {
		fmt.Println("No line containing the string found.")
		return
	}

	// Write the modified lines back to the file
	file, err = os.Create(filePath)
	if err != nil {
		fmt.Println("Error opening file for writing:", err)
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for _, line := range lines {
		_, err := writer.WriteString(line + "\n")
		if err != nil {
			fmt.Println("Error writing to file:", err)
			return
		}
	}
	writer.Flush()
}

func execCmd(command string)  {
	cmd := exec.Command("bash", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run() // add error checking

}

func main()  {

	// get yesterdays date
	currentTime := time.Now().AddDate(0,0,-1)
	currentDate := currentTime.Format("2006_01_02")

	update := flag.String("update", "yesterday", "Set to date yyyy_mm_dd to update the mirror to the snapshot of the defined day or to current day if set to current and update the system (if package list in config changed, the changes will be applied). Important: enter date with leading 0")
	pacconfig := flag.String("pacconfig", "/etc/pacman.conf", "Define the pacman.conf to be used.")
	packagegroup := flag.String("packagegroup", "all", "Define which package groups from the config.json should be installed. Seperate groups by ',', e.g. --packages=gaming,basicprograms")
	// build := flag.Bool("build", true, "Default mode of operation. Only build packages of  the local repo or ones where patches are defined. Update local repository afterwards.")

	flag.Parse()

	// Read patch configuration
	configFilePath := "./configs/config.json"
	config := read_config(configFilePath)

	snapshotDay := ""
	snapshotMonth := ""
	snapshotYear := ""

	// parse date given in update-flag
	if *update == "yesterday" {
		*update = currentDate
	}
	yyyy_mm_dd := *update
	snapshotYear = yyyy_mm_dd[:4]
	snapshotMonth = yyyy_mm_dd[5:7]
	snapshotDay = yyyy_mm_dd[8:10]

	// read all packages to be explicitely installed
	packageGroups := strings.Split(*packagegroup,",")
	fmt.Println(packageGroups)

	// loop through all package groups defined in the startup of nompac
	for group := range packageGroups {
		// loop through all packageGroups defined in the config file
		for _, packages := range config.Packages {
			for key, pkgList := range packages {
				// if the current group from the config file is equal to the group read from input, add packages to the complete package list of the explicitely to be installed packages
				if key == packageGroups[group] || packageGroups[0] == "all" {
					for i := range pkgList {			
						// tolower is necessary to be sure that the sort function below sorts all entries correctly
						packageList = append(packageList, strings.ToLower(pkgList[i]))
					}
				}
			}
		}
	}

	// sort the resulting slice of packages
	sort.Strings(packageList)

	// get currently explicitely installed packages
	var packageListInstalled []string
	command := "pacman -Qe | cut -d' ' -f 1"
	cmd , _ := exec.Command("bash", "-c", command).Output()
	result := string(cmd)
	packageListInstalled = strings.Fields(result)

	var packagesToDelete []string
	var packagesToInstall []string
	// search for packages that are explicitely installed but are not in the list of packages that should be installed
	for i := range packageListInstalled {
		found := 0
		for j := range packageList {
			if packageList[j] == packageListInstalled[i] {
				found = 1
			}
		}
		if found == 0 {
			packagesToDelete = append(packagesToDelete, packageListInstalled[i])
			fmt.Println(packageListInstalled[i])
		}
	}

	// search for packages that are not explicitely installed but are in the list of packages that should be installed
	for i := range packageList {
		found := 0
		for j := range packageListInstalled {
			if packageListInstalled[j] == packageList[i] {
				found = 1
			}
		}
		if found == 0 {
			packagesToInstall = append(packagesToInstall, packageList[i])
			fmt.Println(packageList[i])
		}
	}
	
	fmt.Println(Blue + "Used settings:" + Reset)
	fmt.Println("Local build directory: " + config.BuildDir)
	fmt.Println("Local repository: " + config.LocalRepoDir)
	fmt.Println("Patch directory: " + config.PatchDir)
	fmt.Println("Overlay directory: " + config.OverlayDir)
	fmt.Println("pacman.conf location: " + *pacconfig)
	
	os.MkdirAll(config.BuildDir, os.ModePerm)

	fmt.Println(Blue + "Building patched upstream-packages" + Reset)

	// process and apply patches
	for _, patch := range config.Patches {
	        // Iterate over each patch map
	        for key, patches := range patch {

		    	packageName := key

			// read package version in arch repository
		    	packageVersion := get_current_version_from_repo(packageName)
			fileName := packageName+"-"+packageVersion
			fileExtension := ".tar.gz"
			filePath := filepath.Join(config.BuildDir,fileName)+fileExtension
			
			// read version of installed package
			installedPkgVersion := readInstalledPkgVersion(packageName)

			// only rebuild if version in repository is different than the one installed.
			if strings.Compare(strings.Trim(installedPkgVersion, "\n"), strings.Trim(packageVersion, "\n")) != 0 {
			// get current tar ball
			getCurrentTarballFromRepo(packageName, packageVersion, filePath)
		        
			// extract to build-directory defined in config.json
			extractTarGz(filePath, filepath.Join(config.BuildDir, "src"))
			
			// apply all patches
	            	applyPatches(config, patches, packageName, fileName)

			// build the package
			buildPackage(config, fileName)

			//update personal (local) repository
			updateRepository(config,packageName, fileName)

			//cleanup temporary build directory
			cleanup(config)
			} else {
				fmt.Println("No new version of package " + packageName + " available --> no rebuild")
			}
		}
	}

	fmt.Println(Blue + "Building packages from overlays" + Reset)
	for _, packageName := range config.Overlays {
		// read package version from local package overlay
		packageVersion := get_current_version_from_overlay(config, packageName)
		fileName := packageName+"-"+packageVersion
		// read version of installed package
		installedPkgVersion := readInstalledPkgVersion(packageName)
		// only build package if the package version in the PKGBUILD is different thatn the installed version.
		if strings.Compare(strings.Trim(installedPkgVersion, "\n"), strings.Trim(packageVersion, "\n")) != 0 {
			err := os.CopyFS(config.BuildDir+"/src/"+fileName, os.DirFS(filepath.Join(config.OverlayDir,packageName)))

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

		modifyFile("/home/simonheise/git_repos/nompac/configs/mirrorlist","archive.archlinux.org","Server = https://archive.archlinux.org/repos/"+snapshotYear+"/"+snapshotMonth+"/"+snapshotDay+"/$repo/os/$arch")

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
		command += " --config "+*pacconfig +
			"; sudo DIFFPROG='nvim -d' pacdiff"
		fmt.Println(printInstall)
		execCmd(command)

	}

}
