// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package device

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/device"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/kr/pretty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// pluginName is the deviceName of the plugin
	// this is used for logging and (along with the version) for uniquely identifying
	// plugin binaries fingerprinted by the client
	pluginName = "edge-device"

	// plugin version allows the client to identify and use newer versions of
	// an installed plugin
	pluginVersion = "v0.0.1"

	// vendor is the label for the vendor providing the devices.
	// along with "type" and "model", this can be used when requesting devices:
	//   https://www.nomadproject.io/docs/job-specification/device.html#name
	vendor = "ambarella"

	// deviceType is the "type" of device being returned
	deviceType = "edge"
)

var (
	// pluginInfo provides information used by Nomad to identify the plugin
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDevice,
		PluginApiVersions: []string{device.ApiVersion010},
		PluginVersion:     pluginVersion,
		Name:              pluginName,
	}

	// configSpec is the specification of the schema for this plugin's config.
	// this is used to validate the HCL for the plugin provided
	// as part of the client config:
	//   https://www.nomadproject.io/docs/configuration/plugin.html
	// options are here:
	//   https://github.com/hashicorp/nomad/blob/v0.10.0/plugins/shared/hclspec/hcl_spec.proto
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"chipset":          hclspec.NewAttr("chipset", "string", true),
		"operating_system": hclspec.NewAttr("operating_system", "string", false),
		"rtos_device":      hclspec.NewAttr("rtos_device", "string", false),
		"linux_device":     hclspec.NewAttr("linux_device", "string", false),
		"nfs_server_ip":    hclspec.NewAttr("nfs_server_ip", "string", false),
		"nfs_server_path":  hclspec.NewAttr("nfs_server_path", "string", false),
		"nfs_device_path":  hclspec.NewAttr("nfs_device_path", "string", false),
		// "format_id":        hclspec.NewAttr("format_id", "number", false),
	})
)

// Config contains configuration information for the plugin.
type Config struct {
	Chipset         string `codec:"chipset"`
	OperatingSystem string `codec:"operating_system"`
	RtosDevice      string `codec:"rtos_device"`
	LinuxDevice     string `codec:"linux_device"`
	NfsServerIp     string `codec:"nfs_server_ip"`
	NfsServerPath   string `codec:"nfs_server_path"`
	NfsDevicePath   string `codec:"nfs_device_path"`
	// FormatId        string `codec:"format_id"`
}

// EdgeDevicePlugin contains a skeleton for most of the implementation of a
// device plugin.
type EdgeDevicePlugin struct {
	logger log.Logger

	// these are local copies of the config values that we need for operation
	chipset         string
	operatingSystem string
	rtosDevice      string
	linuxDevice     string
	nfsServerIp     string
	nfsServerPath   string
	nfsDevicePath   string
	// formatId        string

	// devices is a list of fingerprinted devices
	// most plugins will maintain, at least, a list of the devices that were
	// discovered during fingerprinting.
	// we'll save the "device name"/"model"
	devices    map[string]string
	deviceLock sync.RWMutex
}

// NewPlugin returns a device plugin, used primarily by the main wrapper
//
// Plugin configuration isn't available yet, so there will typically be
// a limit to the initialization that can be performed at this point.
func NewPlugin(log log.Logger) *EdgeDevicePlugin {
	return &EdgeDevicePlugin{
		logger:  log.Named(pluginName),
		devices: make(map[string]string),
	}
}

// PluginInfo returns information describing the plugin.
//
// This is called during Nomad client startup, while discovering and loading
// plugins.
func (d *EdgeDevicePlugin) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema returns the configuration schema for the plugin.
//
// This is called during Nomad client startup, immediately before parsing
// plugin config and calling SetConfig
func (d *EdgeDevicePlugin) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig is called by the client to pass the configuration for the plugin.
func (d *EdgeDevicePlugin) SetConfig(c *base.Config) error {

	// decode the plugin config
	var config Config
	if err := base.MsgPackDecode(c.PluginConfig, &config); err != nil {
		return err
	}

	// save the configuration to the plugin
	// typically, we'll perform any additional validation or conversion
	// from MsgPack base types
	if config.Chipset == "" {
		return fmt.Errorf("chipset must be defined")
	}

	d.chipset = config.Chipset
	d.operatingSystem = config.OperatingSystem
	d.linuxDevice = config.LinuxDevice
	d.rtosDevice = config.RtosDevice
	d.nfsServerIp = config.NfsServerIp
	d.nfsServerPath = config.NfsServerPath
	d.nfsDevicePath = config.NfsDevicePath
	// d.formatId = config.FormatId

	d.logger.Info("config set", "config", log.Fmt("% #v", pretty.Formatter(config)))
	return nil
}

// Fingerprint streams detected devices.
// Messages should be emitted to the returned channel when there are changes
// to the devices or their health.
func (d *EdgeDevicePlugin) Fingerprint(ctx context.Context) (<-chan *device.FingerprintResponse, error) {
	// Fingerprint returns a channel. The recommended way of organizing a plugin
	// is to pass that into a long-running goroutine and return the channel immediately.
	outCh := make(chan *device.FingerprintResponse)
	go d.doFingerprint(ctx, outCh)
	return outCh, nil
}

// Stats streams statistics for the detected devices.
// Messages should be emitted to the returned channel on the specified interval.
func (d *EdgeDevicePlugin) Stats(ctx context.Context, interval time.Duration) (<-chan *device.StatsResponse, error) {
	// Similar to Fingerprint, Stats returns a channel. The recommended way of
	// organizing a plugin is to pass that into a long-running goroutine and
	// return the channel immediately.
	outCh := make(chan *device.StatsResponse)
	go d.doStats(ctx, outCh, interval)
	return outCh, nil
}

type reservationError struct {
	notExistingIDs []string
}

func (e *reservationError) Error() string {
	return fmt.Sprintf("unknown device IDs: %s", strings.Join(e.notExistingIDs, ","))
}

// Reserve returns information to the task driver on on how to mount the given devices.
// It may also perform any device-specific orchestration necessary to prepare the device
// for use. This is called in a pre-start hook on the client, before starting the workload.
func (d *EdgeDevicePlugin) Reserve(deviceIDs []string) (*device.ContainerReservation, error) {
	if len(deviceIDs) == 0 {
		return &device.ContainerReservation{}, nil
	}

	// This pattern can be useful for some drivers to avoid a race condition where a device disappears
	// after being scheduled by the server but before the server gets an update on the fingerprint
	// channel that the device is no longer available.
	d.deviceLock.RLock()
	var notExistingIDs []string
	for _, id := range deviceIDs {
		if _, deviceIDExists := d.devices[id]; !deviceIDExists {
			notExistingIDs = append(notExistingIDs, id)
		}
	}
	d.deviceLock.RUnlock()
	if len(notExistingIDs) != 0 {
		return nil, &reservationError{notExistingIDs}
	}

	// initialize the response
	resp := &device.ContainerReservation{
		Envs:    map[string]string{},
		Mounts:  []*device.Mount{},
		Devices: []*device.DeviceSpec{},
	}

	// Mounts are used to mount host volumes into a container that may include
	// libraries, etc.
	resp.Mounts = append(resp.Mounts, &device.Mount{
		TaskPath: "/usr/lib/libsome-library.so",
		HostPath: "/usr/lib/libprobably-some-fingerprinted-or-configured-library.so",
		ReadOnly: true,
	})

	for i, id := range deviceIDs {
		// Check if the device is known
		if _, ok := d.devices[id]; !ok {
			return nil, status.Newf(codes.InvalidArgument, "unknown device %q", id).Err()
		}

		// Envs are a set of environment variables to set for the task.
		resp.Envs[fmt.Sprintf("DEVICE_%d", i)] = id

		// Devices are the set of devices to mount into the container.
		resp.Devices = append(resp.Devices,
			&device.DeviceSpec{
				TaskPath:    d.rtosDevice,
				HostPath:    d.rtosDevice,
				CgroupPerms: "rx",
			},
			&device.DeviceSpec{
				TaskPath:    d.linuxDevice,
				HostPath:    d.linuxDevice,
				CgroupPerms: "rx",
			},
		)
	}

	return resp, nil
}
