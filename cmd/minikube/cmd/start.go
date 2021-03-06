/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/docker/machine/libmachine/host"
	"github.com/docker/machine/libmachine/state"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
	cmdcfg "k8s.io/minikube/cmd/minikube/cmd/config"
	cmdutil "k8s.io/minikube/cmd/util"
	"k8s.io/minikube/pkg/minikube/cluster"
	cfg "k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/machine"
	pkgutil "k8s.io/minikube/pkg/util"
	"k8s.io/minikube/pkg/util/kubeconfig"
	"k8s.io/minikube/pkg/version"
)

const (
	isoURL                = "iso-url"
	memory                = "memory"
	cpus                  = "cpus"
	humanReadableDiskSize = "disk-size"
	vmDriver              = "vm-driver"
	xhyveDiskDriver       = "xhyve-disk-driver"
	NFSSharesRoot         = "nfs-shares-root"
	NFSShare              = "nfs-share"
	kubernetesVersion     = "kubernetes-version"
	hostOnlyCIDR          = "host-only-cidr"
	containerRuntime      = "container-runtime"
	criSocket             = "cri-socket"
	networkPlugin         = "network-plugin"
	enableDefaultCNI      = "enable-default-cni"
	hypervVirtualSwitch   = "hyperv-virtual-switch"
	kvmNetwork            = "kvm-network"
	keepContext           = "keep-context"
	createMount           = "mount"
	featureGates          = "feature-gates"
	apiServerName         = "apiserver-name"
	apiServerPort         = "apiserver-port"
	dnsDomain             = "dns-domain"
	serviceCIDR           = "service-cluster-ip-range"
	mountString           = "mount-string"
	disableDriverMounts   = "disable-driver-mounts"
	cacheImages           = "cache-images"
	uuid                  = "uuid"
	vpnkitSock            = "hyperkit-vpnkit-sock"
	vsockPorts            = "hyperkit-vsock-ports"
	gpu                   = "gpu"
	embedCerts            = "embed-certs"
)

var (
	registryMirror   []string
	dockerEnv        []string
	dockerOpt        []string
	insecureRegistry []string
	apiServerNames   []string
	apiServerIPs     []net.IP
	extraOptions     pkgutil.ExtraOptionSlice
)

// startCmd represents the start command
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Starts a local kubernetes cluster",
	Long: `Starts a local kubernetes cluster using VM. This command
assumes you have already installed one of the VM drivers: virtualbox/vmwarefusion/kvm/xhyve/hyperv.`,
	Run: runStart,
}

// SetContainerRuntime possibly sets the container runtime
func SetContainerRuntime(cfg map[string]string, runtime string) map[string]string {
	switch runtime {
	case "crio", "cri-o":
		cfg["runtime-endpoint"] = "unix:///var/run/crio/crio.sock"
		cfg["image-endpoint"] = "unix:///var/run/crio/crio.sock"
	case "containerd":
		cfg["runtime-endpoint"] = "unix:///run/containerd/containerd.sock"
		cfg["image-endpoint"] = "unix:///run/containerd/containerd.sock"
	default:
		return nil
	}

	return cfg
}

func runStart(cmd *cobra.Command, args []string) {
	if glog.V(8) {
		glog.Infoln("Viper configuration:")
		viper.Debug()
	}
	shouldCacheImages := viper.GetBool(cacheImages)
	k8sVersion := viper.GetString(kubernetesVersion)
	clusterBootstrapper := viper.GetString(cmdcfg.Bootstrapper)

	var groupCacheImages errgroup.Group
	if shouldCacheImages {
		groupCacheImages.Go(func() error {
			return machine.CacheImagesForBootstrapper(k8sVersion, clusterBootstrapper)
		})
	}

	api, err := machine.NewAPIClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting client: %v\n", err)
		os.Exit(1)
	}
	defer api.Close()

	exists, err := api.Exists(cfg.GetMachineName())
	if err != nil {
		glog.Exitf("checking if machine exists: %v", err)
	}

	diskSize := viper.GetString(humanReadableDiskSize)
	diskSizeMB := pkgutil.CalculateDiskSizeInMB(diskSize)

	if diskSizeMB < constants.MinimumDiskSizeMB {
		err := fmt.Errorf("Disk Size %dMB (%s) is too small, the minimum disk size is %dMB", diskSizeMB, diskSize, constants.MinimumDiskSizeMB)
		glog.Errorln("Error parsing disk size:", err)
		os.Exit(1)
	}

	if viper.GetBool(gpu) && viper.GetString(vmDriver) != "kvm2" {
		glog.Exitf("--gpu is only supported with --vm-driver=kvm2")
	}

	config := cfg.MachineConfig{
		MinikubeISO:         viper.GetString(isoURL),
		Memory:              viper.GetInt(memory),
		CPUs:                viper.GetInt(cpus),
		DiskSize:            diskSizeMB,
		VMDriver:            viper.GetString(vmDriver),
		ContainerRuntime:    viper.GetString(containerRuntime),
		HyperkitVpnKitSock:  viper.GetString(vpnkitSock),
		HyperkitVSockPorts:  viper.GetStringSlice(vsockPorts),
		XhyveDiskDriver:     viper.GetString(xhyveDiskDriver),
		NFSShare:            viper.GetStringSlice(NFSShare),
		NFSSharesRoot:       viper.GetString(NFSSharesRoot),
		DockerEnv:           dockerEnv,
		DockerOpt:           dockerOpt,
		InsecureRegistry:    insecureRegistry,
		RegistryMirror:      registryMirror,
		HostOnlyCIDR:        viper.GetString(hostOnlyCIDR),
		HypervVirtualSwitch: viper.GetString(hypervVirtualSwitch),
		KvmNetwork:          viper.GetString(kvmNetwork),
		Downloader:          pkgutil.DefaultDownloader{},
		DisableDriverMounts: viper.GetBool(disableDriverMounts),
		UUID:                viper.GetString(uuid),
		GPU:                 viper.GetBool(gpu),
	}

	// Load current profile cluster config from file, before overwriting it with the new state
	oldConfig, err := cfg.Load()
	if err != nil && !os.IsNotExist(err) {
		glog.Errorln("Error loading profile config: ", err)
	}

	// Write profile cluster configuration to file
	clusterConfig := cfg.Config{
		MachineConfig: config,
	}

	if err := saveConfig(clusterConfig); err != nil {
		glog.Errorln("Error saving profile cluster configuration: ", err)
	}

	fmt.Printf("Starting local Kubernetes %s cluster...\n", viper.GetString(kubernetesVersion))
	fmt.Println("Starting VM...")
	var host *host.Host
	start := func() (err error) {
		host, err = cluster.StartHost(api, config)
		if err != nil {
			glog.Errorf("Error starting host: %v.\n\n Retrying.\n", err)
		}
		return err
	}
	err = pkgutil.RetryAfter(5, start, 2*time.Second)
	if err != nil {
		glog.Errorln("Error starting host: ", err)
		cmdutil.MaybeReportErrorAndExit(err)
	}

	fmt.Println("Getting VM IP address...")
	ip, err := host.Driver.GetIP()
	if err != nil {
		glog.Errorln("Error getting VM IP address: ", err)
		cmdutil.MaybeReportErrorAndExit(err)
	}

	// common config (currently none)
	var cricfg = map[string]string{}
	selectedContainerRuntime := viper.GetString(containerRuntime)
	if cricfg := SetContainerRuntime(cricfg, selectedContainerRuntime); cricfg != nil {
		var command string
		fmt.Println("Writing crictl config...")
		if command, err = cmdutil.GetCrictlConfigCommand(cricfg); err == nil {
			_, err = host.RunSSHCommand(command)
		}
		if err != nil {
			glog.Errorln("Error writing crictl config: ", err)
		}
	}

	selectedKubernetesVersion := viper.GetString(kubernetesVersion)
	if strings.Compare(selectedKubernetesVersion, "") == 0 {
		selectedKubernetesVersion = constants.DefaultKubernetesVersion
	}
	if oldConfig != nil {
		oldKubernetesVersion, err := semver.Make(strings.TrimPrefix(oldConfig.KubernetesConfig.KubernetesVersion, version.VersionPrefix))
		if err != nil {
			glog.Errorln("Error parsing version semver: ", err)
		}

		newKubernetesVersion, err := semver.Make(strings.TrimPrefix(viper.GetString(kubernetesVersion), version.VersionPrefix))
		if err != nil {
			glog.Errorln("Error parsing version semver: ", err)
		}

		// Check if it's an attempt to downgrade version. Avoid version downgrad.
		if newKubernetesVersion.LT(oldKubernetesVersion) {
			selectedKubernetesVersion = version.VersionPrefix + oldKubernetesVersion.String()
			fmt.Println("Kubernetes version downgrade is not supported. Using version:", selectedKubernetesVersion)
		}
	}

	kubernetesConfig := cfg.KubernetesConfig{
		KubernetesVersion:      selectedKubernetesVersion,
		NodeIP:                 ip,
		NodePort:               viper.GetInt(apiServerPort),
		NodeName:               constants.DefaultNodeName,
		APIServerName:          viper.GetString(apiServerName),
		APIServerNames:         apiServerNames,
		APIServerIPs:           apiServerIPs,
		DNSDomain:              viper.GetString(dnsDomain),
		FeatureGates:           viper.GetString(featureGates),
		ContainerRuntime:       selectedContainerRuntime,
		CRISocket:              viper.GetString(criSocket),
		NetworkPlugin:          viper.GetString(networkPlugin),
		ServiceCIDR:            viper.GetString(serviceCIDR),
		ExtraOptions:           extraOptions,
		ShouldLoadCachedImages: shouldCacheImages,
		EnableDefaultCNI:       viper.GetBool(enableDefaultCNI),
	}

	k8sBootstrapper, err := GetClusterBootstrapper(api, clusterBootstrapper)
	if err != nil {
		glog.Exitf("Error getting cluster bootstrapper: %v", err)
	}

	// Write profile cluster configuration to file
	clusterConfig = cfg.Config{
		MachineConfig:    config,
		KubernetesConfig: kubernetesConfig,
	}

	if err := saveConfig(clusterConfig); err != nil {
		glog.Errorln("Error saving profile cluster configuration: ", err)
	}

	if shouldCacheImages {
		fmt.Println("Waiting for image caching to complete...")
		if err := groupCacheImages.Wait(); err != nil {
			glog.Errorln("Error caching images: ", err)
		}
	}

	fmt.Println("Moving files into cluster...")

	if err := k8sBootstrapper.UpdateCluster(kubernetesConfig); err != nil {
		glog.Errorln("Error updating cluster: ", err)
		cmdutil.MaybeReportErrorAndExit(err)
	}

	fmt.Println("Setting up certs...")
	if err := k8sBootstrapper.SetupCerts(kubernetesConfig); err != nil {
		glog.Errorln("Error configuring authentication: ", err)
		cmdutil.MaybeReportErrorAndExit(err)
	}

	fmt.Println("Connecting to cluster...")
	kubeHost, err := host.Driver.GetURL()
	if err != nil {
		glog.Errorln("Error connecting to cluster: ", err)
	}
	kubeHost = strings.Replace(kubeHost, "tcp://", "https://", -1)
	kubeHost = strings.Replace(kubeHost, ":2376", ":"+strconv.Itoa(kubernetesConfig.NodePort), -1)

	fmt.Println("Setting up kubeconfig...")
	// setup kubeconfig

	kubeConfigFile := cmdutil.GetKubeConfigPath()

	kubeCfgSetup := &kubeconfig.KubeConfigSetup{
		ClusterName:          cfg.GetMachineName(),
		ClusterServerAddress: kubeHost,
		ClientCertificate:    constants.MakeMiniPath("client.crt"),
		ClientKey:            constants.MakeMiniPath("client.key"),
		CertificateAuthority: constants.MakeMiniPath("ca.crt"),
		KeepContext:          viper.GetBool(keepContext),
		EmbedCerts:           viper.GetBool(embedCerts),
	}
	kubeCfgSetup.SetKubeConfigFile(kubeConfigFile)

	if err := kubeconfig.SetupKubeConfig(kubeCfgSetup); err != nil {
		glog.Errorln("Error setting up kubeconfig: ", err)
		cmdutil.MaybeReportErrorAndExit(err)
	}

	fmt.Println("Stopping extra container runtimes...")

	if config.VMDriver != constants.DriverNone && selectedContainerRuntime != "" {
		if _, err := host.RunSSHCommand("sudo systemctl stop docker"); err == nil {
			_, err = host.RunSSHCommand("sudo systemctl stop docker.socket")
		}
		if err != nil {
			glog.Errorf("Error stopping docker: %v", err)
		}
	}
	if config.VMDriver != constants.DriverNone && (selectedContainerRuntime != constants.CrioRuntime && selectedContainerRuntime != constants.Cri_oRuntime) {
		if _, err := host.RunSSHCommand("sudo systemctl stop crio"); err != nil {
			glog.Errorf("Error stopping crio: %v", err)
		}
	}
	if config.VMDriver != constants.DriverNone && selectedContainerRuntime != constants.RktRuntime {
		if _, err := host.RunSSHCommand("sudo systemctl stop rkt-api"); err == nil {
			_, err = host.RunSSHCommand("sudo systemctl stop rkt-metadata")
		}
		if err != nil {
			glog.Errorf("Error stopping rkt: %v", err)
		}
	}
	if config.VMDriver != constants.DriverNone && selectedContainerRuntime != constants.ContainerdRuntime {
		if _, err = host.RunSSHCommand("sudo systemctl stop containerd"); err != nil {
			glog.Errorf("Error stopping containerd: %v", err)
		}
	}

	if config.VMDriver != constants.DriverNone && (selectedContainerRuntime == constants.CrioRuntime || selectedContainerRuntime == constants.Cri_oRuntime) {
		fmt.Println("Restarting crio runtime...")
		// restart crio so that it can monitor all hook dirs
		if _, err := host.RunSSHCommand("sudo systemctl restart crio"); err != nil {
			glog.Errorf("Error restarting crio: %v", err)
		}
	}

	if config.VMDriver != constants.DriverNone && selectedContainerRuntime == constants.ContainerdRuntime {
		fmt.Println("Restarting containerd runtime...")
		// restart containerd so that it can install all plugins
		if _, err := host.RunSSHCommand("sudo systemctl restart containerd"); err != nil {
			glog.Errorf("Error restarting containerd: %v", err)
		}
	}

	if !exists || config.VMDriver == constants.DriverNone {
		fmt.Println("Starting cluster components...")
		if err := k8sBootstrapper.StartCluster(kubernetesConfig); err != nil {
			glog.Errorf("Error starting cluster: %v", err)
			cmdutil.MaybeReportErrorAndExit(err)
		}
	} else {
		fmt.Println("Machine exists, restarting cluster components...")
		if err := k8sBootstrapper.RestartCluster(kubernetesConfig); err != nil {
			glog.Errorln("Error restarting cluster: ", err)
			cmdutil.MaybeReportErrorAndExit(err)
		}
	}

	// Block until the cluster is healthy.
	fmt.Print("Verifying kubelet health ...")
	kStat := func() (err error) {
		st, err := k8sBootstrapper.GetKubeletStatus()
		if err != nil || st != state.Running.String() {
			fmt.Printf(".")
			return &pkgutil.RetriableError{Err: fmt.Errorf("kubelet unhealthy: %v: %s", err, st)}
		}
		return nil
	}
	err = pkgutil.RetryAfter(20, kStat, 3*time.Second)
	if err != nil {
		fmt.Printf("error: %v", err)
		cmdutil.MaybeReportErrorAndExit(err)
	}
	fmt.Print("\nVerifying apiserver health ...")
	aStat := func() (err error) {
		st, err := k8sBootstrapper.GetApiServerStatus(net.ParseIP(ip))
		if err != nil || st != state.Running.String() {
			fmt.Print(".")
			return &pkgutil.RetriableError{Err: fmt.Errorf("apiserver status=%s err=%v", st, err)}
		}
		return nil
	}

	err = pkgutil.RetryAfter(30, aStat, 10*time.Second)
	if err != nil {
		fmt.Printf("error: %v", err)
		cmdutil.MaybeReportErrorAndExit(err)
	}
	fmt.Println()

	// start 9p server mount
	if viper.GetBool(createMount) {
		fmt.Printf("Setting up hostmount on %s...\n", viper.GetString(mountString))

		path := os.Args[0]
		mountDebugVal := 0
		if glog.V(8) {
			mountDebugVal = 1
		}
		mountCmd := exec.Command(path, "mount", fmt.Sprintf("--v=%d", mountDebugVal), viper.GetString(mountString))
		mountCmd.Env = append(os.Environ(), constants.IsMinikubeChildProcess+"=true")
		if glog.V(8) {
			mountCmd.Stdout = os.Stdout
			mountCmd.Stderr = os.Stderr
		}
		err = mountCmd.Start()
		if err != nil {
			glog.Errorf("Error running command minikube mount %v", err)
			cmdutil.MaybeReportErrorAndExit(err)
		}
		err = ioutil.WriteFile(filepath.Join(constants.GetMinipath(), constants.MountProcessFileName), []byte(strconv.Itoa(mountCmd.Process.Pid)), 0644)
		if err != nil {
			glog.Errorf("Error writing mount process pid to file: %v", err)
			cmdutil.MaybeReportErrorAndExit(err)
		}
	}

	if kubeCfgSetup.KeepContext {
		fmt.Printf("The local Kubernetes cluster has started. The kubectl context has not been altered, kubectl will require \"--context=%s\" to use the local Kubernetes cluster.\n",
			kubeCfgSetup.ClusterName)
	} else {
		fmt.Println("Kubectl is now configured to use the cluster.")
	}

	if config.VMDriver == "none" {
		if viper.GetBool(cfg.WantNoneDriverWarning) {
			fmt.Println(`===================
WARNING: IT IS RECOMMENDED NOT TO RUN THE NONE DRIVER ON PERSONAL WORKSTATIONS
	The 'none' driver will run an insecure kubernetes apiserver as root that may leave the host vulnerable to CSRF attacks` + "\n")
		}

		if os.Getenv("CHANGE_MINIKUBE_NONE_USER") == "" {
			fmt.Println(`When using the none driver, the kubectl config and credentials generated will be root owned and will appear in the root home directory.
You will need to move the files to the appropriate location and then set the correct permissions.  An example of this is below:

	sudo mv /root/.kube $HOME/.kube # this will write over any previous configuration
	sudo chown -R $USER $HOME/.kube
	sudo chgrp -R $USER $HOME/.kube

	sudo mv /root/.minikube $HOME/.minikube # this will write over any previous configuration
	sudo chown -R $USER $HOME/.minikube
	sudo chgrp -R $USER $HOME/.minikube

This can also be done automatically by setting the env var CHANGE_MINIKUBE_NONE_USER=true`)
		}
		if err := pkgutil.MaybeChownDirRecursiveToMinikubeUser(constants.GetMinipath()); err != nil {
			glog.Errorf("Error recursively changing ownership of directory %s: %s",
				constants.GetMinipath(), err)
			cmdutil.MaybeReportErrorAndExit(err)
		}
	}

	fmt.Println("Loading cached images from config file.")
	err = LoadCachedImagesInConfigFile()
	if err != nil {
		fmt.Println("Unable to load cached images from config file.")
	}
	fmt.Println("\n\nEverything looks great. Please enjoy minikube!")
	return
}

func init() {
	startCmd.Flags().Bool(keepContext, constants.DefaultKeepContext, "This will keep the existing kubectl context and will create a minikube context.")
	startCmd.Flags().Bool(createMount, false, "This will start the mount daemon and automatically mount files into minikube")
	startCmd.Flags().String(mountString, constants.DefaultMountDir+":"+constants.DefaultMountEndpoint, "The argument to pass the minikube mount command on start")
	startCmd.Flags().Bool(disableDriverMounts, false, "Disables the filesystem mounts provided by the hypervisors (vboxfs, xhyve-9p)")
	startCmd.Flags().String(isoURL, constants.DefaultIsoUrl, "Location of the minikube iso")
	startCmd.Flags().String(vmDriver, constants.DefaultVMDriver, fmt.Sprintf("VM driver is one of: %v", constants.SupportedVMDrivers))
	startCmd.Flags().Int(memory, constants.DefaultMemory, "Amount of RAM allocated to the minikube VM in MB")
	startCmd.Flags().Int(cpus, constants.DefaultCPUS, "Number of CPUs allocated to the minikube VM")
	startCmd.Flags().String(humanReadableDiskSize, constants.DefaultDiskSize, "Disk size allocated to the minikube VM (format: <number>[<unit>], where unit = b, k, m or g)")
	startCmd.Flags().String(hostOnlyCIDR, "192.168.99.1/24", "The CIDR to be used for the minikube VM (only supported with Virtualbox driver)")
	startCmd.Flags().String(hypervVirtualSwitch, "", "The hyperv virtual switch name. Defaults to first found. (only supported with HyperV driver)")
	startCmd.Flags().String(kvmNetwork, "default", "The KVM network name. (only supported with KVM driver)")
	startCmd.Flags().String(xhyveDiskDriver, "ahci-hd", "The disk driver to use [ahci-hd|virtio-blk] (only supported with xhyve driver)")
	startCmd.Flags().StringSlice(NFSShare, []string{}, "Local folders to share with Guest via NFS mounts (Only supported on with hyperkit now)")
	startCmd.Flags().String(NFSSharesRoot, "/nfsshares", "Where to root the NFS Shares (defaults to /nfsshares, only supported with hyperkit now)")
	startCmd.Flags().StringArrayVar(&dockerEnv, "docker-env", nil, "Environment variables to pass to the Docker daemon. (format: key=value)")
	startCmd.Flags().StringArrayVar(&dockerOpt, "docker-opt", nil, "Specify arbitrary flags to pass to the Docker daemon. (format: key=value)")
	startCmd.Flags().Int(apiServerPort, pkgutil.APIServerPort, "The apiserver listening port")
	startCmd.Flags().String(apiServerName, constants.APIServerName, "The apiserver name which is used in the generated certificate for kubernetes.  This can be used if you want to make the apiserver available from outside the machine")
	startCmd.Flags().StringArrayVar(&apiServerNames, "apiserver-names", nil, "A set of apiserver names which are used in the generated certificate for kubernetes.  This can be used if you want to make the apiserver available from outside the machine")
	startCmd.Flags().IPSliceVar(&apiServerIPs, "apiserver-ips", nil, "A set of apiserver IP Addresses which are used in the generated certificate for kubernetes.  This can be used if you want to make the apiserver available from outside the machine")
	startCmd.Flags().String(dnsDomain, constants.ClusterDNSDomain, "The cluster dns domain name used in the kubernetes cluster")
	startCmd.Flags().String(serviceCIDR, pkgutil.DefaultServiceCIDR, "The CIDR to be used for service cluster IPs.")
	startCmd.Flags().StringSliceVar(&insecureRegistry, "insecure-registry", nil, "Insecure Docker registries to pass to the Docker daemon.  The default service CIDR range will automatically be added.")
	startCmd.Flags().StringSliceVar(&registryMirror, "registry-mirror", nil, "Registry mirrors to pass to the Docker daemon")
	startCmd.Flags().String(containerRuntime, "", "The container runtime to be used")
	startCmd.Flags().String(criSocket, "", "The cri socket path to be used")
	startCmd.Flags().String(kubernetesVersion, constants.DefaultKubernetesVersion, "The kubernetes version that the minikube VM will use (ex: v1.2.3)")
	startCmd.Flags().String(networkPlugin, "", "The name of the network plugin")
	startCmd.Flags().Bool(enableDefaultCNI, false, "Enable the default CNI plugin (/etc/cni/net.d/k8s.conf). Used in conjunction with \"--network-plugin=cni\"")
	startCmd.Flags().String(featureGates, "", "A set of key=value pairs that describe feature gates for alpha/experimental features.")
	startCmd.Flags().Bool(cacheImages, false, "If true, cache docker images for the current bootstrapper and load them into the machine.")
	startCmd.Flags().Var(&extraOptions, "extra-config",
		`A set of key=value pairs that describe configuration that may be passed to different components.
		The key should be '.' separated, and the first part before the dot is the component to apply the configuration to.
		Valid components are: kubelet, apiserver, controller-manager, etcd, proxy, scheduler.`)
	startCmd.Flags().String(uuid, "", "Provide VM UUID to restore MAC address (only supported with Hyperkit driver).")
	startCmd.Flags().String(vpnkitSock, "", "Location of the VPNKit socket used for networking. If empty, disables Hyperkit VPNKitSock, if 'auto' uses Docker for Mac VPNKit connection, otherwise uses the specified VSock.")
	startCmd.Flags().StringSlice(vsockPorts, []string{}, "List of guest VSock ports that should be exposed as sockets on the host (Only supported on with hyperkit now).")
	startCmd.Flags().Bool(gpu, false, "Enable experimental NVIDIA GPU support in minikube (works only with kvm2 driver on Linux)")
	viper.BindPFlags(startCmd.Flags())
	RootCmd.AddCommand(startCmd)
}

// saveConfig saves profile cluster configuration in
// $MINIKUBE_HOME/profiles/<profilename>/config.json
func saveConfig(clusterConfig cfg.Config) error {
	data, err := json.MarshalIndent(clusterConfig, "", "    ")
	if err != nil {
		return err
	}

	profileConfigFile := constants.GetProfileFile(viper.GetString(cfg.MachineProfile))

	if err := os.MkdirAll(filepath.Dir(profileConfigFile), 0700); err != nil {
		return err
	}

	if err := saveConfigToFile(data, profileConfigFile); err != nil {
		return err
	}

	return nil
}

func saveConfigToFile(data []byte, file string) error {
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return ioutil.WriteFile(file, data, 0600)
	}

	tmpfi, err := ioutil.TempFile(filepath.Dir(file), "config.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmpfi.Name())

	if err = ioutil.WriteFile(tmpfi.Name(), data, 0600); err != nil {
		return err
	}

	if err = tmpfi.Close(); err != nil {
		return err
	}

	if err = os.Remove(file); err != nil {
		return err
	}

	if err = os.Rename(tmpfi.Name(), file); err != nil {
		return err
	}
	return nil
}
