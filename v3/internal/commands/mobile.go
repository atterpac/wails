package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type MobileRunOptions struct {
	Config   string `name:"config" description:"Path to the build configuration" default:"build/config.yml"`
	Artifact string `name:"artifact" description:"Path to the built application artifact"`
	Device   string `name:"device" description:"Target device serial or identifier"`
}

type MobileLogsOptions struct {
	Device string `name:"device" description:"Target device serial or identifier"`
	All    bool   `name:"all" description:"Show unfiltered platform logs"`
}

func AndroidDevices() error {
	adb, err := androidADB()
	if err != nil {
		return err
	}
	return runMobileCommand(adb, "devices", "-l")
}

func AndroidDevRun(options *MobileRunOptions) error {
	return androidDeploy(options, true)
}

func AndroidDeploy(options *MobileRunOptions) error {
	return androidDeploy(options, false)
}

func androidDeploy(options *MobileRunOptions, supervise bool) error {
	identity, binary, output, err := mobileProject(options.Config)
	if err != nil {
		return err
	}
	artifact := options.Artifact
	if artifact == "" {
		artifact = filepath.Join(output, binary+".apk")
	}
	adb, err := androidADB()
	if err != nil {
		return err
	}
	if options.Device == "" {
		if err := ensureAndroidEmulator(adb); err != nil {
			return err
		}
	}
	prefix := androidDeviceArguments(options.Device)
	_ = runMobileCommand(adb, append(prefix, "uninstall", identity)...)
	if err := runMobileCommand(adb, append(prefix, "install", "-r", artifact)...); err != nil {
		return err
	}
	if err := runMobileCommand(adb, append(prefix, "shell", "am", "start", "-n", identity+"/com.wails.app.MainActivity")...); err != nil {
		return err
	}
	if supervise {
		return runMobileCommand(adb, append(prefix, "logcat", "-v", "time")...)
	}
	return nil
}

func AndroidLogs(options *MobileLogsOptions) error {
	adb, err := androidADB()
	if err != nil {
		return err
	}
	arguments := append(androidDeviceArguments(options.Device), "logcat", "-v", "time")
	return runMobileCommand(adb, arguments...)
}

func AndroidStudio() error {
	if studio, err := exec.LookPath("studio"); err == nil {
		return runMobileCommand(studio, "build/android")
	}
	if runtime.GOOS == "darwin" {
		return runMobileCommand("open", "-a", "Android Studio", "build/android")
	}
	return fmt.Errorf("Android Studio was not found; open build/android manually")
}

func AndroidClean() error {
	for _, path := range []string{"bin", "build/android/app/build", "build/android/.gradle"} {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	matches, err := filepath.Glob("build/android/app/src/main/jniLibs/*/libwails.so")
	if err != nil {
		return err
	}
	for _, match := range matches {
		if err := os.Remove(match); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func IOSDevices() error {
	return runMobileCommand("xcrun", "devicectl", "list", "devices")
}

func IOSDevRun(options *MobileRunOptions) error {
	return iosDeploy(options, true)
}

func IOSDeploy(options *MobileRunOptions) error {
	return iosDeploy(options, false)
}

func iosDeploy(options *MobileRunOptions, supervise bool) error {
	identity, binary, output, err := mobileProject(options.Config)
	if err != nil {
		return err
	}
	artifact := options.Artifact
	if artifact == "" {
		artifact = filepath.Join(output, binary+".app")
	}
	if options.Device != "" {
		if err := runMobileCommand("xcrun", "devicectl", "device", "install", "app", "--device", options.Device, artifact); err != nil {
			return err
		}
		if err := runMobileCommand("xcrun", "devicectl", "device", "process", "launch", "--device", options.Device, identity); err != nil {
			return err
		}
		if supervise {
			return runMobileCommand("xcrun", "devicectl", "device", "process", "list", "--device", options.Device)
		}
		return nil
	}
	if err := ensureIOSSimulator(); err != nil {
		return err
	}
	_ = runMobileCommand("xcrun", "simctl", "terminate", "booted", identity)
	_ = runMobileCommand("xcrun", "simctl", "uninstall", "booted", identity)
	if err := runMobileCommand("xcrun", "simctl", "install", "booted", artifact); err != nil {
		return err
	}
	if err := runMobileCommand("xcrun", "simctl", "launch", "booted", identity); err != nil {
		return err
	}
	if supervise {
		return iosLogStream(false)
	}
	return nil
}

func IOSLogs(options *MobileLogsOptions) error {
	return iosLogStream(options.All)
}

func IOSXcode() error {
	return runMobileCommand("open", "build/ios/xcode/main.xcodeproj")
}

func iosLogStream(all bool) error {
	arguments := []string{"simctl", "spawn", "booted", "log", "stream", "--level", "debug", "--style", "compact"}
	if !all {
		arguments = append(arguments, "--predicate", `senderImagePath CONTAINS[c] ".app/"`)
	}
	return runMobileCommand("xcrun", arguments...)
}

func androidADB() (string, error) {
	for _, variable := range []string{"ANDROID_SDK_ROOT", "ANDROID_HOME"} {
		if root := os.Getenv(variable); root != "" {
			candidate := filepath.Join(root, "platform-tools", executableName("adb"))
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	path, err := exec.LookPath("adb")
	if err != nil {
		return "", fmt.Errorf("adb was not found; install Android SDK platform-tools or set ANDROID_SDK_ROOT")
	}
	return path, nil
}

func ensureAndroidEmulator(adb string) error {
	if output, err := exec.Command(adb, "devices").Output(); err == nil && strings.Contains(string(output), "emulator-") {
		return nil
	}
	emulator, err := androidSDKTool("emulator", "emulator")
	if err != nil {
		return err
	}
	output, err := exec.Command(emulator, "-list-avds").Output()
	if err != nil {
		return fmt.Errorf("list Android virtual devices: %w", err)
	}
	devices := strings.Fields(string(output))
	if len(devices) == 0 {
		return fmt.Errorf("no Android Virtual Device is configured; create one in Android Studio before running dev mode")
	}
	command := exec.Command(emulator, "-avd", devices[len(devices)-1], "-no-snapshot-load")
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Android emulator: %w", err)
	}
	_ = command.Process.Release()
	if err := runMobileCommand(adb, "wait-for-device"); err != nil {
		return err
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		output, _ := exec.Command(adb, "shell", "getprop", "sys.boot_completed").Output()
		if strings.TrimSpace(string(output)) == "1" {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("Android emulator did not finish booting within 120 seconds")
}

func androidSDKTool(directory, name string) (string, error) {
	for _, variable := range []string{"ANDROID_SDK_ROOT", "ANDROID_HOME"} {
		if root := os.Getenv(variable); root != "" {
			candidate := filepath.Join(root, directory, executableName(name))
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s was not found in the Android SDK or PATH", name)
	}
	return path, nil
}

func ensureIOSSimulator() error {
	if output, err := exec.Command("xcrun", "simctl", "list", "devices", "booted").Output(); err == nil && strings.Contains(string(output), "Booted") {
		return nil
	}
	output, err := exec.Command("xcrun", "simctl", "list", "devices", "available").Output()
	if err != nil {
		return fmt.Errorf("list iOS simulators: %w", err)
	}
	var identifier string
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, "iPhone") {
			continue
		}
		for _, field := range strings.Fields(line) {
			candidate := strings.Trim(field, "()")
			if len(candidate) == 36 && strings.Count(candidate, "-") == 4 {
				identifier = candidate
				break
			}
		}
		if identifier != "" {
			break
		}
	}
	if identifier == "" {
		return fmt.Errorf("no available iPhone simulator was found in Xcode")
	}
	_ = runMobileCommand("xcrun", "simctl", "boot", identifier)
	_ = exec.Command("open", "-a", "Simulator").Start()
	return runMobileCommand("xcrun", "simctl", "bootstatus", identifier, "-b")
}

func androidDeviceArguments(device string) []string {
	if device == "" {
		return nil
	}
	return []string{"-s", device}
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func mobileProject(configPath string) (identity, binary, output string, err error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", "", "", err
	}
	var config struct {
		Info struct {
			ProductIdentifier string `yaml:"productIdentifier"`
			ProductName       string `yaml:"productName"`
		} `yaml:"info"`
		Build struct {
			BinaryName string `yaml:"binaryName"`
			Output     string `yaml:"output"`
		} `yaml:"build"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return "", "", "", err
	}
	identity = config.Info.ProductIdentifier
	if identity == "" {
		return "", "", "", fmt.Errorf("info.productIdentifier is required for mobile deployment")
	}
	binary = config.Build.BinaryName
	if binary == "" {
		binary = normaliseName(config.Info.ProductName)
	}
	output = config.Build.Output
	if output == "" {
		output = "bin"
	}
	return identity, binary, output, nil
}

func runMobileCommand(name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}
