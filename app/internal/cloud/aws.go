// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package cloud

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type EC2 interface {
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeInstanceTypeOfferings(context.Context, *ec2.DescribeInstanceTypeOfferingsInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	CreateSecurityGroup(context.Context, *ec2.CreateSecurityGroupInput, ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(context.Context, *ec2.AuthorizeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupIngress(context.Context, *ec2.RevokeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
	DeleteSecurityGroup(context.Context, *ec2.DeleteSecurityGroupInput, ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// AWS runs bursts on EC2 in one region, in its default VPC.
type AWS struct {
	EC2         EC2
	Account     string
	RunningWait time.Duration
}

// awsTimeout bounds each EC2 call, as Hetzner's client does: the SDK has
// none, and deletes run on contexts that are never cancelled.
var awsTimeout = time.Minute

// NewAWS reads the usual credential chain (environment, profiles, SSO).
func NewAWS(ctx context.Context, region string) (*AWS, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region),
		config.WithHTTPClient(awshttp.NewBuildableClient().WithTimeout(awsTimeout)))
	if err != nil {
		return nil, err
	}
	who, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("aws: who am I: %w", err)
	}
	return &AWS{EC2: ec2.NewFromConfig(cfg), Account: aws.ToString(who.Account), RunningWait: 5 * time.Minute}, nil
}

const debianOwner = "136693071363"

// Live instances: a terminated one lingers in listings for an hour.
var liveStates = []string{"pending", "running", "stopping", "stopped"}

// awsErr makes an EC2 error an APIError, so Refused and Locked read it.
func awsErr(err error) error {
	var resp *smithyhttp.ResponseError
	if err == nil || !errors.As(err, &resp) {
		return err
	}
	e := &APIError{Provider: "aws", Status: resp.HTTPStatusCode(), Sent: resp.HTTPStatusCode(), Message: err.Error()}
	var api smithy.APIError
	if errors.As(err, &api) {
		e.Code, e.Message = api.ErrorCode(), api.ErrorMessage()
	}
	switch e.Code {
	case "InvalidInstanceID.NotFound":
		return fmt.Errorf("%w: %w", ErrNotFound, e)
	// EC2 throttles with 503 or 400: wait and retry, as for Hetzner's 429.
	case "RequestLimitExceeded", "Throttling", "ThrottlingException":
		e.Status = 429
	// Sent as 400s, but the request may have gone through.
	case "RequestTimeout", "PriorRequestNotComplete":
		e.Status = 500
	// Sent as 500s, but asking again now gets the same answer.
	case "InsufficientInstanceCapacity", "InsufficientAddressCapacity", "InsufficientVolumeCapacity", "Unsupported", "VcpuLimitExceeded", "InstanceLimitExceeded":
		e.Status = 400
	}
	return e
}

func (a *AWS) InstanceType(ctx context.Context, name string) (goarch string, err error) {
	out, err := a.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{types.InstanceType(name)}})
	if err != nil {
		return "", awsErr(err)
	}
	if len(out.InstanceTypes) != 1 || out.InstanceTypes[0].ProcessorInfo == nil {
		return "", fmt.Errorf("aws: no instance type %s here", name)
	}
	archs := out.InstanceTypes[0].ProcessorInfo.SupportedArchitectures
	switch {
	case slices.Contains(archs, types.ArchitectureTypeX8664):
		return "amd64", nil
	case slices.Contains(archs, types.ArchitectureTypeArm64):
		return "arm64", nil
	}
	return "", fmt.Errorf("aws: %s runs %v", name, archs)
}

// Subnets lists the default subnets in zones that offer the type, so a
// create can move on to another zone when one has no capacity.
func (a *AWS) Subnets(ctx context.Context, typ string) ([]string, error) {
	const perms = "(needs ec2:DescribeInstanceTypeOfferings and ec2:DescribeSubnets)"
	zones := map[string]bool{}
	offers := ec2.NewDescribeInstanceTypeOfferingsPaginator(a.EC2, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: types.LocationTypeAvailabilityZone,
		Filters:      []types.Filter{{Name: aws.String("instance-type"), Values: []string{typ}}},
	})
	for offers.HasMorePages() {
		page, err := offers.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing zones %s: %w", perms, awsErr(err))
		}
		for _, o := range page.InstanceTypeOfferings {
			zones[aws.ToString(o.Location)] = true
		}
	}
	var ids []string
	subnets := ec2.NewDescribeSubnetsPaginator(a.EC2, &ec2.DescribeSubnetsInput{Filters: []types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}}})
	for subnets.HasMorePages() {
		page, err := subnets.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing subnets %s: %w", perms, awsErr(err))
		}
		for _, sn := range page.Subnets {
			if zones[aws.ToString(sn.AvailabilityZone)] {
				ids = append(ids, aws.ToString(sn.SubnetId))
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("aws: no default subnet in a zone that offers %s", typ)
	}
	slices.Sort(ids)
	return ids, nil
}

// Image finds the newest Debian 13 AMI for an architecture, or checks that
// image is one of Debian's, and returns its root device.
func (a *AWS) Image(ctx context.Context, image, goarch string) (id, root string, err error) {
	in := &ec2.DescribeImagesInput{Owners: []string{debianOwner}}
	if strings.HasPrefix(image, "ami-") {
		in.ImageIds = []string{image}
	} else {
		arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[goarch]
		in.Filters = []types.Filter{
			{Name: aws.String("name"), Values: []string{image + "-" + goarch + "-2*"}},
			{Name: aws.String("architecture"), Values: []string{arch}},
			{Name: aws.String("state"), Values: []string{"available"}},
		}
	}
	out, err := a.EC2.DescribeImages(ctx, in)
	if err != nil {
		return "", "", awsErr(err)
	}
	if len(out.Images) == 0 {
		return "", "", fmt.Errorf("aws: no Debian image %s for %s", image, goarch)
	}
	// ISO 8601, so the strings sort by time.
	newest := slices.MaxFunc(out.Images, func(x, y types.Image) int {
		return strings.Compare(aws.ToString(x.CreationDate), aws.ToString(y.CreationDate))
	})
	return aws.ToString(newest.ImageId), aws.ToString(newest.RootDeviceName), nil
}

func (a *AWS) Firewall(ctx context.Context, name string, sources []string) (string, error) {
	for _, c := range sources {
		if p, err := netip.ParsePrefix(c); err != nil || !p.Addr().Is4() {
			return "", fmt.Errorf("aws: ssh_from %s: IPv4 only, as instances get no IPv6 address", c)
		}
	}
	vpcs, err := a.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []types.Filter{{Name: aws.String("is-default"), Values: []string{"true"}}}})
	if err != nil {
		return "", awsErr(err)
	}
	if len(vpcs.Vpcs) != 1 {
		return "", fmt.Errorf("aws: bursts need the region's default VPC")
	}
	vpc := vpcs.Vpcs[0].VpcId
	groups, err := a.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []types.Filter{
		{Name: aws.String("group-name"), Values: []string{name}},
		{Name: aws.String("vpc-id"), Values: []string{aws.ToString(vpc)}},
	}})
	if err != nil {
		return "", awsErr(err)
	}
	var id string
	var have []types.IpPermission
	if len(groups.SecurityGroups) == 1 {
		g := groups.SecurityGroups[0]
		if !slices.ContainsFunc(g.Tags, func(t types.Tag) bool { return aws.ToString(t.Key) == Label && aws.ToString(t.Value) == "firewall" }) {
			return "", fmt.Errorf("aws: security group %s isn't tagged %s=firewall, so it isn't ours to change", name, Label)
		}
		id, have = aws.ToString(g.GroupId), g.IpPermissions
	} else {
		made, err := a.EC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName: aws.String(name), Description: aws.String("m2test burst workers: SSH from ssh_from only"), VpcId: vpc,
			TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeSecurityGroup, Tags: []types.Tag{{Key: aws.String(Label), Value: aws.String("firewall")}}}},
		})
		if err != nil {
			return "", awsErr(err)
		}
		id = aws.ToString(made.GroupId)
	}
	// missing: still to add. Everything else in the group (old CIDRs,
	// other ports) is revoked.
	missing := map[string]bool{}
	for _, c := range sources {
		missing[c] = true
	}
	var stale []types.IpPermission
	for _, p := range have {
		ssh := aws.ToString(p.IpProtocol) == "tcp" && aws.ToInt32(p.FromPort) == 22 && aws.ToInt32(p.ToPort) == 22
		var gone []types.IpRange
		for _, r := range p.IpRanges {
			if c := aws.ToString(r.CidrIp); ssh && missing[c] {
				delete(missing, c)
			} else {
				gone = append(gone, r)
			}
		}
		p.IpRanges = gone
		if len(p.IpRanges)+len(p.Ipv6Ranges)+len(p.PrefixListIds)+len(p.UserIdGroupPairs) > 0 {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		if _, err := a.EC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(id), IpPermissions: stale}); err != nil {
			return "", awsErr(err)
		}
	}
	add := types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22)}
	for _, c := range sources {
		if missing[c] {
			add.IpRanges = append(add.IpRanges, types.IpRange{CidrIp: aws.String(c)})
			delete(missing, c)
		}
	}
	if len(add.IpRanges) > 0 {
		if _, err := a.EC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(id), IpPermissions: []types.IpPermission{add}}); err != nil {
			return "", awsErr(err)
		}
	}
	return id, nil
}

// DeletePool removes the pool's security group if tagged as ours; one still on an instance, even a recently terminated one, can't go yet.
func (a *AWS) DeletePool(ctx context.Context, firewall, _ string) error {
	groups, err := a.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []types.Filter{
		{Name: aws.String("group-name"), Values: []string{firewall}},
		{Name: aws.String("tag:" + Label), Values: []string{"firewall"}},
	}})
	if err != nil {
		return awsErr(err)
	}
	for _, g := range groups.SecurityGroups {
		if _, err := a.EC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: g.GroupId}); err != nil {
			return fmt.Errorf("aws: security group %s: %w", aws.ToString(g.GroupId), awsErr(err))
		}
	}
	return nil
}

func tags(labels map[string]string, name string) []types.Tag {
	t := []types.Tag{{Key: aws.String("Name"), Value: aws.String(name)}}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		t = append(t, types.Tag{Key: aws.String(k), Value: aws.String(labels[k])})
	}
	return t
}

func instance(i types.Instance) Server {
	s := Server{ID: aws.ToString(i.InstanceId), IP: aws.ToString(i.PublicIpAddress), Labels: map[string]string{}}
	if i.State != nil {
		s.Status = string(i.State.Name)
	}
	for _, t := range i.Tags {
		if k := aws.ToString(t.Key); k == "Name" {
			s.Name = aws.ToString(t.Value)
		} else {
			s.Labels[k] = aws.ToString(t.Value)
		}
	}
	return s
}

// Create runs one instance and waits for its public address. The nonce label doubles as the client token, so a retry makes no second.
// Metadata accepts only IMDSv2 tokens, which can't cross a hop into a container.
func (a *AWS) Create(ctx context.Context, s Spec) (Server, error) {
	subnets := s.Subnets
	if len(subnets) == 0 {
		subnets = []string{""} // EC2 picks
	}
	var out *ec2.RunInstancesOutput
	var err error
	nonce := s.Labels[Label+"-nonce"]
	// Started at a different zone per worker, so a burst spreads out.
	first := int(crc32.ChecksumIEEE([]byte(nonce)) % uint32(len(subnets)))
	for i := range subnets {
		// A token EC2 has seen can't carry other parameters.
		token := nonce
		if i > 0 {
			token += "-" + strconv.Itoa(i)
		}
		out, err = a.run(ctx, s, subnets[(first+i)%len(subnets)], token)
		var api *APIError
		if !errors.As(err, &api) || api.Code != "InsufficientInstanceCapacity" && api.Code != "Unsupported" {
			break
		}
	}
	if err != nil {
		return Server{}, err
	}
	if len(out.Instances) != 1 {
		return Server{}, fmt.Errorf("aws: asked for one instance, told of %d", len(out.Instances))
	}
	return a.wait(ctx, instance(out.Instances[0]))
}

func (a *AWS) run(ctx context.Context, s Spec, subnet, token string) (*ec2.RunInstancesOutput, error) {
	t := tags(s.Labels, s.Name)
	nic := types.InstanceNetworkInterfaceSpecification{
		DeviceIndex: aws.Int32(0), AssociatePublicIpAddress: aws.Bool(true), Groups: []string{s.Firewall}, DeleteOnTermination: aws.Bool(true),
	}
	if subnet != "" {
		nic.SubnetId = aws.String(subnet)
	}
	out, err := a.EC2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: aws.String(s.Image), InstanceType: types.InstanceType(s.Type),
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		ClientToken:                       aws.String(token),
		UserData:                          aws.String(base64.StdEncoding.EncodeToString([]byte(s.UserData))),
		NetworkInterfaces:                 []types.InstanceNetworkInterfaceSpecification{nic},
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpEndpoint: types.InstanceMetadataEndpointStateEnabled, HttpTokens: types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1), InstanceMetadataTags: types.InstanceMetadataTagsStateDisabled,
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{{DeviceName: aws.String(s.RootDevice), Ebs: &types.EbsBlockDevice{
			VolumeSize: aws.Int32(s.DiskGB), VolumeType: types.VolumeTypeGp3, DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true),
		}}},
		TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeInstance, Tags: t}, {ResourceType: types.ResourceTypeVolume, Tags: t}},
	})
	return out, awsErr(err)
}

func (a *AWS) wait(ctx context.Context, srv Server) (Server, error) {
	started := time.Now()
	wait, cancel := context.WithTimeout(ctx, a.RunningWait)
	defer cancel()
	for srv.IP == "" || srv.Status != string(types.InstanceStateNameRunning) {
		select {
		case <-wait.Done():
			return srv, fmt.Errorf("instance %s made, but not running with a public address after %s (%s)", srv.ID, a.RunningWait, srv.Status)
		case <-time.After(ActionPoll):
		}
		got, err := a.Get(wait, srv.ID)
		switch {
		// EC2 itself saying no such ID: not yet visible to reads, as it
		// warns may happen at first. Terminated is not this.
		case errors.As(err, new(*APIError)) && errors.Is(err, ErrNotFound) && time.Since(started) < time.Minute:
		// Throttled or a blip: the instance is likely fine.
		case err != nil && wait.Err() == nil && (Locked(err) || !Refused(err)):
		case err != nil:
			return srv, fmt.Errorf("instance %s made, but %w", srv.ID, err)
		default:
			srv = got
		}
	}
	return srv, nil
}

func (a *AWS) Get(ctx context.Context, id string) (Server, error) {
	out, err := a.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return Server{}, awsErr(err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if s := instance(i); s.ID == id && slices.Contains(liveStates, s.Status) {
				return s, nil
			}
		}
	}
	return Server{}, ErrNotFound
}

func (a *AWS) Delete(ctx context.Context, id string) error {
	_, err := a.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err = awsErr(err); errors.Is(err, ErrNotFound) {
		// A new instance may not show yet; one long gone, Get reports.
		return fmt.Errorf("aws: %s not found to terminate, try again", id)
	}
	return err
}

func (a *AWS) List(ctx context.Context, selector string) ([]Server, error) {
	filters := []types.Filter{{Name: aws.String("instance-state-name"), Values: liveStates}}
	for _, term := range strings.Split(selector, ",") {
		k, v, ok := strings.Cut(term, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("aws: bad selector %q", selector)
		}
		filters = append(filters, types.Filter{Name: aws.String("tag:" + k), Values: []string{v}})
	}
	var all []Server
	pages := ec2.NewDescribeInstancesPaginator(a.EC2, &ec2.DescribeInstancesInput{Filters: filters})
	for pages.HasMorePages() {
		out, err := pages.NextPage(ctx)
		if err != nil {
			return nil, awsErr(err)
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				all = append(all, instance(i))
			}
		}
	}
	return all, nil
}
