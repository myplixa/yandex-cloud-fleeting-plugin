package fleeting_plugin_yc

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"runtime"
	"sync"
	"text/template"

	"github.com/ghodss/yaml"
	"github.com/google/uuid"
	"github.com/hashicorp/go-hclog"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1/instancegroup"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

type InstanceGroup struct {
	FolderId           string `json:"folder_id"`
	Name               string `json:"name"`
	ConfigInstanceFile string `json:"config_file"`
	SshPrivateKeyFile  string `json:"ssh_file,omitempty"`
	// KeyFile is an authorized key of a service account in the JSON format
	// written by `yc iam key create --output`. See credentials.
	KeyFile string `json:"key_file,omitempty"`

	SshPublicKey    string
	InstanceGroupId string

	size     int64
	ycsdk    *ycsdk.SDK
	settings provider.Settings

	template InstanceTemplate

	// nameToID caches ManagedInstance.Name -> ManagedInstance.Id; see resolveInstanceID.
	nameToID sync.Map

	cloudInitExtraTemplate *template.Template

	logger hclog.Logger
}

// zoneIDs returns the configured zones, preferring the plural Zones field
// over the legacy singular Zone for backward compatibility with existing
// template.yml files that only set `zone:`.
func (g *InstanceGroup) zoneIDs() []string {
	if len(g.template.Zones) > 0 {
		return g.template.Zones
	}
	return []string{g.template.Zone}
}

// subnetIDs returns the configured subnets, preferring the plural Subnets
// field over the legacy singular Subnet for backward compatibility. Each
// entry must belong to one of the zones returned by zoneIDs - YC's Instance
// Group service pairs each subnet to its own zone, not by list position.
func (g *InstanceGroup) subnetIDs() []string {
	if len(g.template.Network.Subnets) > 0 {
		return g.template.Network.Subnets
	}
	return []string{g.template.Network.Subnet}
}

func (g *InstanceGroup) GenerateInstanceGroup() *instancegroup.InstanceTemplate {
	instanceGroupTemplateNetwork := &instancegroup.NetworkInterfaceSpec{
		NetworkId:            g.template.Network.Network,
		SubnetIds:            g.subnetIDs(),
		PrimaryV4AddressSpec: &instancegroup.PrimaryAddressSpec{},
	}

	if g.template.Network.Nat {
		instanceGroupTemplateNetwork.PrimaryV4AddressSpec = &instancegroup.PrimaryAddressSpec{OneToOneNatSpec: &instancegroup.OneToOneNatSpec{IpVersion: instancegroup.IpVersion_IPV4}}
	}

	if len(g.template.Network.Security) > 0 {
		instanceGroupTemplateNetwork.SecurityGroupIds = []string{g.template.Network.Security}
	}

	var listInstanceGroupTemplateNetwork []*instancegroup.NetworkInterfaceSpec

	listInstanceGroupTemplateNetwork = append(listInstanceGroupTemplateNetwork, instanceGroupTemplateNetwork)

	instanceGroupTemplate := &instancegroup.InstanceTemplate{
		Name:       g.Name + "-" + uuid.New().String()[:8] + "-{instance.index}",
		Labels:     g.GenerateLabels(),
		PlatformId: g.template.Platform,
		ResourcesSpec: &instancegroup.ResourcesSpec{
			Cores:        g.template.Resources.CPU,
			Memory:       g.template.Resources.Memory,
			CoreFraction: g.template.Resources.Fraction,
		},
		BootDiskSpec: &instancegroup.AttachedDiskSpec{
			Mode: instancegroup.AttachedDiskSpec_READ_WRITE,
			DiskSpec: &instancegroup.AttachedDiskSpec_DiskSpec{
				Size:   g.template.Disk.Size,
				TypeId: g.template.Disk.Type,
				SourceOneof: &instancegroup.AttachedDiskSpec_DiskSpec_ImageId{
					ImageId: g.template.Disk.Image,
				},
			},
		},
		NetworkInterfaceSpecs: listInstanceGroupTemplateNetwork,
		SchedulingPolicy:      &instancegroup.SchedulingPolicy{Preemptible: g.template.Preemptible},
		Metadata:              g.GenerateMeta(),
	}

	if g.template.VmServiceAccount != "" {
		instanceGroupTemplate.ServiceAccountId = g.template.VmServiceAccount
	}

	return instanceGroupTemplate
}

func (g *InstanceGroup) GenerateInstanceGroupRequest() *instancegroup.CreateInstanceGroupRequest {

	instanceTemplate := g.GenerateInstanceGroup()
	var instanceGroupTemplateZone []*instancegroup.AllocationPolicy_Zone
	for _, zoneID := range g.zoneIDs() {
		instanceGroupTemplateZone = append(instanceGroupTemplateZone, &instancegroup.AllocationPolicy_Zone{ZoneId: zoneID})
	}
	instanceGroupCreateReq := &instancegroup.CreateInstanceGroupRequest{
		FolderId:         g.FolderId,
		Name:             g.Name,
		InstanceTemplate: instanceTemplate,
		ScalePolicy:      &instancegroup.ScalePolicy{ScaleType: &instancegroup.ScalePolicy_FixedScale_{FixedScale: &instancegroup.ScalePolicy_FixedScale{Size: 0}}},
		DeployPolicy: &instancegroup.DeployPolicy{
			MaxCreating:    3,
			MaxUnavailable: 1,
			MaxDeleting:    1,
		},
		AllocationPolicy: &instancegroup.AllocationPolicy{Zones: instanceGroupTemplateZone},
		ServiceAccountId: g.template.ServiceAccount,
	}

	return instanceGroupCreateReq
}

func (g *InstanceGroup) GenerateUpdateInstanceGroupRequest() *instancegroup.UpdateInstanceGroupRequest {
	instanceTemplate := g.GenerateInstanceGroup()
	var instanceGroupTemplateZone []*instancegroup.AllocationPolicy_Zone
	for _, zoneID := range g.zoneIDs() {
		instanceGroupTemplateZone = append(instanceGroupTemplateZone, &instancegroup.AllocationPolicy_Zone{ZoneId: zoneID})
	}
	instanceGroupCreateReq := &instancegroup.UpdateInstanceGroupRequest{
		InstanceGroupId:  g.InstanceGroupId,
		Name:             g.Name,
		InstanceTemplate: instanceTemplate,
		ScalePolicy:      &instancegroup.ScalePolicy{ScaleType: &instancegroup.ScalePolicy_FixedScale_{FixedScale: &instancegroup.ScalePolicy_FixedScale{Size: 0}}},
		DeployPolicy: &instancegroup.DeployPolicy{
			MaxCreating:    3,
			MaxUnavailable: 1,
			MaxDeleting:    1,
		},
		AllocationPolicy: &instancegroup.AllocationPolicy{Zones: instanceGroupTemplateZone},
		ServiceAccountId: g.template.ServiceAccount,
	}

	return instanceGroupCreateReq

}

func (g *InstanceGroup) GenerateMeta() map[string]string {
	metaData := make(map[string]string)

	metaData["ssh-keys"] = g.settings.Username + ":" + g.SshPublicKey

	var parts []string

	if g.template.Monitoring.Enabled {
		metaData["install-unified-agent"] = "1"
		parts = append(parts, "#cloud-config\nruncmd:\n  - wget -O - https://monitoring.api.cloud.yandex.net/monitoring/v2/unifiedAgent/config/install.sh | bash\n")
	}

	if g.cloudInitExtraTemplate != nil {
		extra, err := renderCloudInitExtra(g.cloudInitExtraTemplate, g.Name, g.template.CloudInitVars)
		if err != nil {
			g.logger.Error("rendering cloud_init_extra_file failed, skipping it for this metadata generation", "error", err)
		} else {
			parts = append(parts, extra)
		}
	}

	switch len(parts) {
	case 0:
	case 1:
		metaData["user-data"] = parts[0]
	default:
		combined, err := buildMultipartUserData(parts...)
		if err != nil {
			g.logger.Error("assembling multipart cloud-init user data failed, falling back to first part only", "error", err)
			metaData["user-data"] = parts[0]
		} else {
			metaData["user-data"] = combined
		}
	}

	return metaData
}

func (g *InstanceGroup) GenerateLabels() map[string]string {

	labels := make(map[string]string)

	labels["fleet-runner"] = "true"
	labels["fleet-runner-name"] = g.Name

	return labels
}

// credentials picks how the plugin authenticates to the YC API: with the
// service account key from key_file when it is set, so the plugin can run
// outside Yandex Cloud, otherwise with the service account attached to the
// VM the plugin runs on, read from the metadata service.
func (g *InstanceGroup) credentials() (ycsdk.Credentials, error) {
	if g.KeyFile == "" {
		return ycsdk.InstanceServiceAccount(), nil
	}

	g.warnIfKeyFileTooOpen()

	key, err := iamkey.ReadFromJSONFile(g.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading key_file %q: %w", g.KeyFile, err)
	}

	creds, err := ycsdk.ServiceAccountKey(key)
	if err != nil {
		return nil, fmt.Errorf("key_file %q is not a usable service account key: %w", g.KeyFile, err)
	}

	return creds, nil
}

// warnIfKeyFileTooOpen logs a warning when key_file is readable by group or
// other. The key can create and delete VMs across the whole cloud, so it
// should be readable only by the user GitLab Runner runs as. It never refuses
// to start, and the check is skipped on Windows where mode bits carry no
// meaning.
func (g *InstanceGroup) warnIfKeyFileTooOpen() {
	if g.logger == nil || runtime.GOOS == "windows" {
		return
	}

	info, err := os.Stat(g.KeyFile)
	if err != nil {
		return // a missing/unreadable file is reported by the caller
	}

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		g.logger.Warn("key_file is readable by group or other, restrict it with chmod 600",
			"key_file", g.KeyFile, "mode", fmt.Sprintf("%#o", perm))
	}
}

func (g *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g.logger = logger

	creds, err := g.credentials()
	if err != nil {
		return provider.ProviderInfo{}, err
	}

	sdk, err := ycsdk.Build(ctx, ycsdk.Config{
		Credentials: creds,
	})

	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("building YC SDK client: %w", err)
	}

	g.settings = settings
	log.Println("Running with settings: ", g.settings)
	pemBytes, err := os.ReadFile(g.SshPrivateKeyFile)

	if err != nil {
		log.Println("Not set private SSH file")
	}

	var iTemplate InstanceTemplate
	ymlData, err := os.ReadFile(g.ConfigInstanceFile)

	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("reading config_file %q: %w", g.ConfigInstanceFile, err)
	}

	err = yaml.Unmarshal(ymlData, &iTemplate)

	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("parsing config_file %q: %w", g.ConfigInstanceFile, err)
	}

	g.template = iTemplate

	if iTemplate.CloudInitExtraFile != "" {
		tmpl, err := loadCloudInitExtraTemplate(iTemplate.CloudInitExtraFile)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("loading cloud_init_extra_file: %w", err)
		}
		g.cloudInitExtraTemplate = tmpl
	}

	g.settings.Key = pemBytes
	g.size = 0

	info := provider.ConnectInfo{ConnectorConfig: g.settings.ConnectorConfig}

	err = g.ssh(&info)

	if err != nil {
		return provider.ProviderInfo{}, err
	}

	instanceGroupListResp, err := sdk.InstanceGroup().InstanceGroup().List(ctx, &instancegroup.ListInstanceGroupsRequest{FolderId: g.FolderId})

	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("listing instance groups: %w", err)
	}

	for _, ig := range instanceGroupListResp.InstanceGroups {
		if ig.Name == g.Name {
			g.InstanceGroupId = ig.Id
			g.size = ig.GetManagedInstancesState().GetTargetSize()
		}
	}

	if len(g.InstanceGroupId) < 1 {
		op, err := sdk.WrapOperation(sdk.InstanceGroup().InstanceGroup().Create(ctx, g.GenerateInstanceGroupRequest()))

		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("creating instance group: %w", err)
		}

		err = op.Wait(ctx)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("waiting for instance group creation: %w", err)
		}

		response, _ := op.Response()
		proto := response.ProtoReflect()
		instanceGroupId := response.ProtoReflect().Get(proto.Descriptor().Fields().ByName("id"))

		g.InstanceGroupId = instanceGroupId.String()
	} else {
		log.Println("Use exist IG with ID", g.InstanceGroupId, "name", g.Name, "current size", g.size)
		op, err := sdk.WrapOperation(sdk.InstanceGroup().InstanceGroup().Update(ctx, g.GenerateUpdateInstanceGroupRequest()))
		log.Println("IG updated", g.InstanceGroupId, g.Name)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("updating instance group: %w", err)
		}

		err = op.Wait(ctx)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("waiting for instance group update: %w", err)
		}
	}

	g.ycsdk = sdk

	return provider.ProviderInfo{
		ID:        path.Join("yc", g.FolderId, g.Name),
		MaxSize:   100,
		Version:   VERSION,
		BuildInfo: BuildInfo(),
	}, nil

}

func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	instances, err := g.ycsdk.InstanceGroup().InstanceGroup().ListInstances(ctx,
		&instancegroup.ListInstanceGroupInstancesRequest{
			InstanceGroupId: g.InstanceGroupId,
		})

	if err != nil {
		return err
	}

	for _, instance := range instances.Instances {
		state := provider.StateCreating
		switch instance.Status.String() {
		case "PREPARING_RESOURCES", "CREATING_INSTANCE", "STARTING_INSTANCE":
			state = provider.StateCreating
		case "DELETING_INSTANCE", "STOPPING_INSTANCE":
			state = provider.StateDeleting
		case "DELETED":
			state = provider.StateDeleted
		case "RUNNING_ACTUAL":
			state = provider.StateRunning
		}
		g.nameToID.Store(instance.Name, instance.Id)
		update(instance.Name, state)
	}

	return nil
}

// resolveInstanceID resolves a managed instance Name back to its real YC
// managed-instance Id, falling back to a live ListInstances call on a cache miss.
func (g *InstanceGroup) resolveInstanceID(ctx context.Context, name string) (string, error) {
	if id, ok := g.nameToID.Load(name); ok {
		return id.(string), nil
	}

	instances, err := g.ycsdk.InstanceGroup().InstanceGroup().ListInstances(ctx,
		&instancegroup.ListInstanceGroupInstancesRequest{
			InstanceGroupId: g.InstanceGroupId,
		})
	if err != nil {
		return "", fmt.Errorf("resolving instance id for %q: %w", name, err)
	}

	for _, instance := range instances.Instances {
		g.nameToID.Store(instance.Name, instance.Id)
		if instance.Name == name {
			return instance.Id, nil
		}
	}

	return "", fmt.Errorf("no instance found with name %q", name)
}

func (g *InstanceGroup) Increase(ctx context.Context, delta int) (succeeded int, err error) {
	var updateMaskPath []string
	log.Println("Increase on ", delta, " current IG size", g.size)
	updateMaskPath = append(updateMaskPath, "instance_template.metadata")
	updateMaskPath = append(updateMaskPath, "scale_policy")
	newSizeInstanceGroup := g.size + int64(delta)

	increaseInstanceReq := instancegroup.UpdateInstanceGroupRequest{
		InstanceGroupId:  g.InstanceGroupId,
		InstanceTemplate: &instancegroup.InstanceTemplate{Metadata: g.GenerateMeta()},
		ScalePolicy:      &instancegroup.ScalePolicy{ScaleType: &instancegroup.ScalePolicy_FixedScale_{FixedScale: &instancegroup.ScalePolicy_FixedScale{Size: newSizeInstanceGroup}}},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: updateMaskPath},
	}

	op, err := g.ycsdk.WrapOperation(g.ycsdk.InstanceGroup().InstanceGroup().Update(ctx, &increaseInstanceReq))

	if err != nil {
		log.Println(err)
		return 0, err
	}

	err = op.Wait(ctx)
	if err != nil {
		log.Println(err)
		return 0, err
	}

	g.size = newSizeInstanceGroup
	log.Println("Increased IG new size: ", g.size)
	return delta, nil
}

func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) (succeeded []string, err error) {

	managedIDs := make([]string, 0, len(instances))
	for _, name := range instances {
		id, resolveErr := g.resolveInstanceID(ctx, name)
		if resolveErr != nil {
			log.Println("Decrease: skipping instance, could not resolve id:", resolveErr)
			continue
		}
		managedIDs = append(managedIDs, id)
	}

	if len(managedIDs) == 0 {
		return nil, fmt.Errorf("could not resolve any of the requested instances to a managed-instance id: %v", instances)
	}

	instanceGroupDeleteReq := instancegroup.DeleteInstancesRequest{
		InstanceGroupId:    g.InstanceGroupId,
		ManagedInstanceIds: managedIDs,
	}

	op, err := g.ycsdk.WrapOperation(g.ycsdk.InstanceGroup().InstanceGroup().DeleteInstances(ctx, &instanceGroupDeleteReq))

	if err != nil {
		log.Println(err)
		return nil, err
	}
	err = op.Wait(ctx)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	for _, name := range instances {
		g.nameToID.Delete(name)
	}
	log.Println("Decrease on", len(managedIDs), "current IG size", g.size)
	g.size = g.size - int64(len(managedIDs))
	if g.size < 0 {
		g.size = 0
	}
	log.Println("Decreased new IG size", g.size)
	return instances, nil

}

func (g *InstanceGroup) ConnectInfo(ctx context.Context, instanceId string) (provider.ConnectInfo, error) {
	info := provider.ConnectInfo{ConnectorConfig: g.settings.ConnectorConfig}

	instances, err := g.ycsdk.InstanceGroup().InstanceGroup().ListInstances(ctx,
		&instancegroup.ListInstanceGroupInstancesRequest{
			InstanceGroupId: g.InstanceGroupId,
		})
	if err != nil {
		return provider.ConnectInfo{}, err
	}
	for _, instance := range instances.Instances {
		if instance.Name == instanceId {
			g.nameToID.Store(instance.Name, instance.Id)
			if instance.Status.String() != "RUNNING_ACTUAL" {
				return provider.ConnectInfo{}, fmt.Errorf("instance status is not running (%s)", instance.Status.String())
			}
			ipAddress := instance.NetworkInterfaces[0].GetPrimaryV4Address()

			info.InternalAddr = ipAddress.Address
			info.ExternalAddr = ipAddress.OneToOneNat.GetAddress()
		}
	}
	log.Println("Connecting to instance ", instanceId, "with IP address: ", info.ExternalAddr, info.InternalAddr)
	return info, nil
}

func (g *InstanceGroup) Heartbeat(ctx context.Context, instanceID string) error {
	instances, err := g.ycsdk.InstanceGroup().InstanceGroup().ListInstances(ctx,
		&instancegroup.ListInstanceGroupInstancesRequest{
			InstanceGroupId: g.InstanceGroupId,
		})
	if err != nil {
		return fmt.Errorf("heartbeat: listing instances: %w", err)
	}

	for _, instance := range instances.Instances {
		if instance.Name != instanceID {
			continue
		}
		if instance.Status.String() != "RUNNING_ACTUAL" {
			return fmt.Errorf("heartbeat: instance %s status is %s: %w", instanceID, instance.Status.String(), provider.ErrInstanceUnhealthy)
		}
		return nil
	}

	return fmt.Errorf("heartbeat: instance %s not found: %w", instanceID, provider.ErrInstanceUnhealthy)
}

// Suspend/Resume are unused: ProviderInfo.Capabilities never advertises
// CapabilitySuspendResume, so taskscaler never calls them.
func (g *InstanceGroup) Suspend(ctx context.Context, instances []string) (succeeded []string, err error) {
	return nil, provider.ErrCapabilityNotSupported
}

func (g *InstanceGroup) Resume(ctx context.Context, instances []string) (succeeded []string, err error) {
	return nil, provider.ErrCapabilityNotSupported
}

func (g *InstanceGroup) Shutdown(ctx context.Context) error {

	increaseInstanceReq := instancegroup.UpdateInstanceGroupRequest{
		InstanceGroupId: g.InstanceGroupId,
		ScalePolicy:     &instancegroup.ScalePolicy{ScaleType: &instancegroup.ScalePolicy_FixedScale_{FixedScale: &instancegroup.ScalePolicy_FixedScale{Size: 0}}},
		UpdateMask:      &fieldmaskpb.FieldMask{Paths: []string{"scale_policy"}},
	}

	op, err := g.ycsdk.WrapOperation(g.ycsdk.InstanceGroup().InstanceGroup().Update(ctx, &increaseInstanceReq))

	if err != nil {
		log.Println(err)
		return err
	}

	err = op.Wait(ctx)
	if err != nil {
		log.Println(err)
		return err
	}

	return nil
}
