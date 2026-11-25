// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package cloud

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func sdkErr(status int, code string) error {
	return &smithy.OperationError{ServiceID: "EC2", OperationName: "Op", Err: &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
			Err:      &smithy.GenericAPIError{Code: code, Message: "said no"},
		},
	}}
}

type fakeEC2 struct {
	mu        sync.Mutex
	EC2       // anything not faked panics
	run       *ec2.RunInstancesInput
	runErr    error
	sgErr     error
	runErrs   []error // each run in turn, before runErr
	runs      []*ec2.RunInstancesInput
	zones     []string          // offering the type
	subnets   map[string]string // zone: default subnet
	states    []string          // what each DescribeInstances of the new instance answers
	hidden    int               // DescribeInstances by ID says NotFound this many times first
	throttled int               // then throttles this many times
	blips     int               // then times out this many times
	described int
	listIn    []*ec2.DescribeInstancesInput
	terminate []string
	images    []types.Image
	imagesIn  *ec2.DescribeImagesInput
	archs     []types.ArchitectureType
	vpcs      int
	groups    []types.SecurityGroup
	created   *ec2.CreateSecurityGroupInput
	revoked   []types.IpPermission
	added     []types.IpPermission
	revokes   int
	adds      int
	groupsIn  *ec2.DescribeSecurityGroupsInput
	deleted   []string
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run = in
	f.runs = append(f.runs, in)
	if len(f.runErrs) > 0 {
		err := f.runErrs[0]
		f.runErrs = f.runErrs[1:]
		return nil, err
	}
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-0123456789abcdef0"), State: &types.InstanceState{Name: "pending"}}}}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(in.InstanceIds) == 0 {
		f.listIn = append(f.listIn, in)
		id, next := "i-00000000000000001", aws.String("p2")
		if aws.ToString(in.NextToken) == "p2" {
			id, next = "i-00000000000000002", nil
		}
		return &ec2.DescribeInstancesOutput{NextToken: next, Reservations: []types.Reservation{{Instances: []types.Instance{{
			InstanceId: aws.String(id), State: &types.InstanceState{Name: "running"}, PublicIpAddress: aws.String("198.51.100.1"),
			Tags: []types.Tag{{Key: aws.String("Name"), Value: aws.String("aws-" + id[len(id)-1:])}, {Key: aws.String(Label), Value: aws.String("worker")}},
		}}}}}, nil
	}
	if f.hidden > 0 {
		f.hidden--
		return nil, sdkErr(400, "InvalidInstanceID.NotFound")
	}
	if f.throttled > 0 {
		f.throttled--
		return nil, sdkErr(503, "RequestLimitExceeded")
	}
	if f.blips > 0 {
		f.blips--
		return nil, sdkErr(400, "RequestTimeout")
	}
	state := "terminated"
	if f.described < len(f.states) {
		state = f.states[f.described]
	}
	f.described++
	i := types.Instance{InstanceId: aws.String(in.InstanceIds[0]), State: &types.InstanceState{Name: types.InstanceStateName(state)}}
	if state == "running" {
		i.PublicIpAddress = aws.String("198.51.100.7")
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{i}}}}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminate = append(f.terminate, in.InstanceIds...)
	if in.InstanceIds[0] == "i-0000000000000dead" {
		return nil, sdkErr(400, "InvalidInstanceID.NotFound")
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	f.imagesIn = in
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}

func (f *fakeEC2) DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []types.InstanceTypeInfo{{ProcessorInfo: &types.ProcessorInfo{SupportedArchitectures: f.archs}}}}, nil
}

func (f *fakeEC2) DescribeInstanceTypeOfferings(_ context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	out := &ec2.DescribeInstanceTypeOfferingsOutput{}
	if in.LocationType != types.LocationTypeAvailabilityZone || in.Filters[0].Values[0] != "m7a.2xlarge" {
		return out, nil
	}
	at := 0
	if in.NextToken != nil {
		at, _ = strconv.Atoi(*in.NextToken)
	}
	if at < len(f.zones) {
		out.InstanceTypeOfferings = []types.InstanceTypeOffering{{Location: aws.String(f.zones[at])}}
		if at+1 < len(f.zones) {
			out.NextToken = aws.String(strconv.Itoa(at + 1))
		}
	}
	return out, nil
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	out := &ec2.DescribeSubnetsOutput{}
	if aws.ToString(in.Filters[0].Name) != "default-for-az" {
		return out, nil
	}
	// One subnet a page, to prove every page is read.
	zones := slices.Sorted(maps.Keys(f.subnets))
	at := 0
	if in.NextToken != nil {
		at, _ = strconv.Atoi(*in.NextToken)
	}
	if at < len(zones) {
		out.Subnets = []types.Subnet{{AvailabilityZone: aws.String(zones[at]), SubnetId: aws.String(f.subnets[zones[at]])}}
		if at+1 < len(zones) {
			out.NextToken = aws.String(strconv.Itoa(at + 1))
		}
	}
	return out, nil
}

func (f *fakeEC2) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	out := &ec2.DescribeVpcsOutput{}
	for range f.vpcs {
		out.Vpcs = append(out.Vpcs, types.Vpc{VpcId: aws.String("vpc-1")})
	}
	return out, nil
}

func (f *fakeEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.groupsIn = in
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.groups}, nil
}

func (f *fakeEC2) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	f.created = in
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-new")}, nil
}

func (f *fakeEC2) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.adds++
	f.added = append(f.added, in.IpPermissions...)
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (f *fakeEC2) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	f.revokes++
	f.revoked = append(f.revoked, in.IpPermissions...)
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

func fastPoll(t *testing.T) {
	old := ActionPoll
	ActionPoll = time.Millisecond
	t.Cleanup(func() { ActionPoll = old })
}

func awsSpec() Spec {
	return Spec{Name: "aws-abc", Type: "m7a.2xlarge", Image: "ami-1", RootDevice: "/dev/xvda", DiskGB: 160, Firewall: "sg-1",
		UserData: "#cloud-config\n", Labels: map[string]string{Label: "worker", Label + "-pool": "a1b2c3d4e5f6", Label + "-nonce": "abcdef12"}}
}

func TestAWSCreateAsksForASafeInstanceAndWaitsForItsAddress(t *testing.T) {
	fastPoll(t)
	f := &fakeEC2{states: []string{"pending", "running"}, hidden: 2, throttled: 2, blips: 2}
	a := &AWS{EC2: f, RunningWait: time.Second}
	srv, err := a.Create(context.Background(), awsSpec())
	if err != nil || srv.ID != "i-0123456789abcdef0" || srv.IP != "198.51.100.7" {
		t.Fatalf("got %+v %v", srv, err)
	}
	in := f.run
	ud, _ := base64.StdEncoding.DecodeString(aws.ToString(in.UserData))
	m := in.MetadataOptions
	bd := in.BlockDeviceMappings[0]
	switch {
	case aws.ToString(in.ClientToken) != "abcdef12":
		t.Errorf("client token %q, not the nonce", aws.ToString(in.ClientToken))
	case in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate:
		t.Error("a power-off would only stop it")
	case m == nil || m.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(m.HttpPutResponseHopLimit) != 1:
		t.Errorf("metadata %+v", m)
	case in.IamInstanceProfile != nil || in.KeyName != nil:
		t.Error("instance profile or key pair")
	case len(in.NetworkInterfaces) != 1 || !slices.Equal(in.NetworkInterfaces[0].Groups, []string{"sg-1"}) || !aws.ToBool(in.NetworkInterfaces[0].AssociatePublicIpAddress) || in.SecurityGroupIds != nil:
		t.Errorf("network %+v", in.NetworkInterfaces)
	case !aws.ToBool(in.NetworkInterfaces[0].DeleteOnTermination):
		t.Error("its network interface outlives it")
	case aws.ToString(bd.DeviceName) != "/dev/xvda" || aws.ToInt32(bd.Ebs.VolumeSize) != 160 || !aws.ToBool(bd.Ebs.DeleteOnTermination) || !aws.ToBool(bd.Ebs.Encrypted) || bd.Ebs.VolumeType != types.VolumeTypeGp3:
		t.Errorf("disk %+v %+v", bd, bd.Ebs)
	case string(ud) != "#cloud-config\n":
		t.Errorf("user data %q", ud)
	case len(in.TagSpecifications) != 2:
		t.Errorf("tags on %d resources", len(in.TagSpecifications))
	}
	got := map[string]string{}
	for _, tag := range in.TagSpecifications[0].Tags {
		got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if got["Name"] != "aws-abc" || got[Label+"-nonce"] != "abcdef12" || got[Label+"-pool"] != "a1b2c3d4e5f6" {
		t.Errorf("tags %v", got)
	}
}

func TestAWSCreateReportsAnInstanceThatNeverRuns(t *testing.T) {
	fastPoll(t)
	// Gone at once (capacity, a bad image): its ID still comes back.
	f := &fakeEC2{states: []string{"terminated"}}
	a := &AWS{EC2: f, RunningWait: time.Minute}
	start := time.Now()
	srv, err := a.Create(context.Background(), awsSpec())
	if err == nil || srv.ID != "i-0123456789abcdef0" || time.Since(start) > 5*time.Second {
		t.Fatalf("got %+v %v after %s", srv, err, time.Since(start))
	}
	// Refused mid-poll: no point waiting.
	f = &fakeEC2{states: []string{"pending"}}
	a = &AWS{EC2: &refusingGet{f}, RunningWait: time.Minute}
	start = time.Now()
	if srv, err := a.Create(context.Background(), awsSpec()); err == nil || srv.ID == "" || time.Since(start) > 5*time.Second {
		t.Fatalf("refused: %+v %v", srv, err)
	}
	// Stuck pending.
	f = &fakeEC2{states: slices.Repeat([]string{"pending"}, 1000)}
	a = &AWS{EC2: f, RunningWait: 50 * time.Millisecond}
	if srv, err := a.Create(context.Background(), awsSpec()); err == nil || srv.ID == "" {
		t.Fatalf("got %+v %v", srv, err)
	}
}

func TestAWSRefusalsAndUnknowns(t *testing.T) {
	a := &AWS{EC2: &fakeEC2{runErr: sdkErr(400, "VcpuLimitExceeded")}, RunningWait: time.Second}
	srv, err := a.Create(context.Background(), awsSpec())
	if !Refused(err) || srv.ID != "" || !strings.Contains(err.Error(), "aws: VcpuLimitExceeded (400)") {
		t.Fatalf("400: %+v %v", srv, err)
	}
	a.EC2 = &fakeEC2{runErr: sdkErr(503, "InternalError")}
	if _, err := a.Create(context.Background(), awsSpec()); err == nil || Refused(err) {
		t.Fatalf("503 taken as a refusal: %v", err)
	}
	if !errors.Is(awsErr(sdkErr(400, "InvalidInstanceID.NotFound")), ErrNotFound) {
		t.Error("not found")
	}
	for _, code := range []string{"InvalidInstanceID.Malformed", "InvalidAMIID.NotFound"} {
		if errors.Is(awsErr(sdkErr(400, code)), ErrNotFound) {
			t.Errorf("%s taken as a gone instance", code)
		}
	}
	for _, status := range []int{400, 503} {
		if err := awsErr(sdkErr(status, "RequestLimitExceeded")); !Locked(err) {
			t.Errorf("throttled %d: not retried: %v", status, err)
		}
	}
	if err := awsErr(sdkErr(500, "InsufficientInstanceCapacity")); !strings.Contains(err.Error(), "(500)") {
		t.Errorf("%v doesn't say what EC2 sent", err)
	}
	if err := awsErr(sdkErr(500, "InsufficientInstanceCapacity")); !Refused(err) || Locked(err) {
		t.Errorf("no capacity, asked again: %v", err)
	}
	for _, code := range []string{"RequestTimeout", "PriorRequestNotComplete"} {
		if err := awsErr(sdkErr(400, code)); Refused(err) {
			t.Errorf("%s may have gone through, but is taken as refused", code)
		}
	}
	if err := awsErr(sdkErr(500, "InternalError")); Refused(err) {
		t.Errorf("a 500 not retried: %v", err)
	}
}

func TestAWSGetAndDelete(t *testing.T) {
	f := &fakeEC2{states: []string{"running", "shutting-down", "terminated"}}
	a := &AWS{EC2: f}
	if s, err := a.Get(context.Background(), "i-0123456789abcdef0"); err != nil || s.Status != "running" {
		t.Fatalf("running: %+v %v", s, err)
	}
	// Terminating stops the bill; terminated lingers in listings.
	for _, state := range f.states[1:] {
		if _, err := a.Get(context.Background(), "i-0123456789abcdef0"); !errors.Is(err, ErrNotFound) || errors.As(err, new(*APIError)) {
			t.Fatalf("%s: %v", state, err)
		}
	}
	// It may be new and not yet visible: not taken as gone.
	if err := a.Delete(context.Background(), "i-0000000000000dead"); err == nil || errors.Is(err, ErrNotFound) || Refused(err) {
		t.Fatalf("not found to terminate: %v", err)
	}
	if err := a.Delete(context.Background(), "i-0123456789abcdef0"); err != nil || f.terminate[1] != "i-0123456789abcdef0" {
		t.Fatalf("terminate %v %v", f.terminate, err)
	}
}

func TestAWSListFiltersByTagAndLiveState(t *testing.T) {
	f := &fakeEC2{}
	got, err := (&AWS{EC2: f}).List(context.Background(), Label+"=worker,"+Label+"-pool=a1b2c3d4e5f6")
	if err != nil || len(got) != 2 || got[0].Name != "aws-1" || got[1].Labels[Label] != "worker" {
		t.Fatalf("got %+v %v", got, err)
	}
	want := map[string][]string{"instance-state-name": liveStates, "tag:" + Label: {"worker"}, "tag:" + Label + "-pool": {"a1b2c3d4e5f6"}}
	filters := map[string][]string{}
	for _, fl := range f.listIn[0].Filters {
		filters[aws.ToString(fl.Name)] = fl.Values
	}
	for k, v := range want {
		if !slices.Equal(filters[k], v) {
			t.Errorf("filter %s = %v, want %v", k, filters[k], v)
		}
	}
	if slices.Contains(filters["instance-state-name"], "terminated") || slices.Contains(filters["instance-state-name"], "shutting-down") {
		t.Error("gone instances listed")
	}
	for _, bad := range []string{"m2test", "m2test=", "=worker"} {
		if _, err := (&AWS{EC2: f}).List(context.Background(), bad); err == nil {
			t.Errorf("%q would list every instance", bad)
		}
	}
}

func TestAWSImageAndArch(t *testing.T) {
	f := &fakeEC2{archs: []types.ArchitectureType{"x86_64"}, images: []types.Image{
		{ImageId: aws.String("ami-old"), CreationDate: aws.String("2026-09-01T00:00:00.000Z"), RootDeviceName: aws.String("/dev/xvda")},
		{ImageId: aws.String("ami-new"), CreationDate: aws.String("2026-11-01T00:00:00.000Z"), RootDeviceName: aws.String("/dev/xvda")},
	}}
	a := &AWS{EC2: f}
	if arch, err := a.InstanceType(context.Background(), "m7a.2xlarge"); err != nil || arch != "amd64" {
		t.Fatalf("arch %q %v", arch, err)
	}
	id, root, err := a.Image(context.Background(), "debian-13", "amd64")
	if err != nil || id != "ami-new" || root != "/dev/xvda" {
		t.Fatalf("image %q %q %v", id, root, err)
	}
	if !slices.Equal(f.imagesIn.Owners, []string{debianOwner}) || aws.ToString(f.imagesIn.Filters[0].Name) != "name" || f.imagesIn.Filters[0].Values[0] != "debian-13-amd64-2*" {
		t.Errorf("asked %+v", f.imagesIn)
	}
	if _, _, err := a.Image(context.Background(), "ami-0abc", "amd64"); err != nil || !slices.Equal(f.imagesIn.ImageIds, []string{"ami-0abc"}) || !slices.Equal(f.imagesIn.Owners, []string{debianOwner}) {
		t.Errorf("named AMI: %+v %v", f.imagesIn, err)
	}
	f.images = nil
	if _, _, err := a.Image(context.Background(), "debian-13", "amd64"); err == nil {
		t.Error("no image found, no error")
	}
}

func TestAWSFirewallKeepsOnlySSHFromTheSources(t *testing.T) {
	ours := []types.Tag{{Key: aws.String(Label), Value: aws.String("firewall")}}
	ssh := func(cidrs ...string) types.IpPermission {
		p := types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22)}
		for _, c := range cidrs {
			p.IpRanges = append(p.IpRanges, types.IpRange{CidrIp: aws.String(c)})
		}
		return p
	}
	web := types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(80), ToPort: aws.Int32(80), IpRanges: []types.IpRange{{CidrIp: aws.String("198.51.100.4/32")}}}
	f := &fakeEC2{vpcs: 1, groups: []types.SecurityGroup{{GroupId: aws.String("sg-1"), Tags: ours, IpPermissions: []types.IpPermission{web, ssh("198.51.100.4/32", "203.0.113.0/24")}}}}
	id, err := (&AWS{EC2: f}).Firewall(context.Background(), "m2test-a1b2c3d4e5f6", []string{"198.51.100.4/32", "192.0.2.9/32"})
	if err != nil || id != "sg-1" || f.created != nil {
		t.Fatalf("%q %v, created %+v", id, err, f.created)
	}
	var revoked, added []string
	for _, p := range f.revoked {
		for _, r := range p.IpRanges {
			revoked = append(revoked, aws.ToString(p.IpProtocol)+":"+string(rune('0'+aws.ToInt32(p.FromPort)/10))+":"+aws.ToString(r.CidrIp))
		}
	}
	for _, p := range f.added {
		for _, r := range p.IpRanges {
			added = append(added, aws.ToString(r.CidrIp))
		}
	}
	if !slices.Equal(revoked, []string{"tcp:8:198.51.100.4/32", "tcp:2:203.0.113.0/24"}) || !slices.Equal(added, []string{"192.0.2.9/32"}) {
		t.Fatalf("revoked %v, added %v", revoked, added)
	}
	// Already right: nothing to change.
	f = &fakeEC2{vpcs: 1, groups: []types.SecurityGroup{{GroupId: aws.String("sg-1"), Tags: ours, IpPermissions: []types.IpPermission{ssh("192.0.2.9/32")}}}}
	if _, err := (&AWS{EC2: f}).Firewall(context.Background(), "m2test-x", []string{"192.0.2.9/32"}); err != nil || f.adds+f.revokes != 0 {
		t.Fatalf("added %v, revoked %v, %v", f.added, f.revoked, err)
	}
	f = &fakeEC2{vpcs: 1}
	if id, err := (&AWS{EC2: f}).Firewall(context.Background(), "m2test-x", []string{"192.0.2.9/32"}); err != nil || id != "sg-new" || f.created == nil || len(f.added) != 1 {
		t.Fatalf("new group: %q %v", id, err)
	}
	// Someone else's group by that name is left alone.
	f = &fakeEC2{vpcs: 1, groups: []types.SecurityGroup{{GroupId: aws.String("sg-1"), IpPermissions: []types.IpPermission{web}}}}
	if _, err := (&AWS{EC2: f}).Firewall(context.Background(), "m2test-x", []string{"192.0.2.9/32"}); err == nil || f.adds+f.revokes != 0 {
		t.Fatalf("untagged group changed: %v", err)
	}
	f = &fakeEC2{vpcs: 1}
	if _, err := (&AWS{EC2: f}).Firewall(context.Background(), "m2test-x", []string{"192.0.2.9/32", "::ffff:192.0.2.1/128"}); err == nil || f.created != nil {
		t.Error("IPv6 source taken, or a group made first")
	}
	if _, err := (&AWS{EC2: &fakeEC2{}}).Firewall(context.Background(), "m2test-x", nil); err == nil {
		t.Error("no default VPC, no error")
	}
}

type refusingGet struct{ *fakeEC2 }

func (r *refusingGet) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return nil, sdkErr(403, "UnauthorizedOperation")
}

func TestAWSSubnetsOnlyWhereTheTypeIsOffered(t *testing.T) {
	f := &fakeEC2{zones: []string{"eu-west-1c", "eu-west-1a"}, subnets: map[string]string{"eu-west-1a": "subnet-a", "eu-west-1b": "subnet-b", "eu-west-1c": "subnet-c"}}
	a := &AWS{EC2: f}
	if got, err := a.Subnets(context.Background(), "m7a.2xlarge"); err != nil || !slices.Equal(got, []string{"subnet-a", "subnet-c"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := a.Subnets(context.Background(), "x9.nothing"); err == nil {
		t.Fatalf("offered nowhere: %v", got)
	}
}

func TestAWSCreateMovesOnFromAZoneWithoutCapacity(t *testing.T) {
	fastPoll(t)
	s := awsSpec()
	s.Subnets = []string{"subnet-a", "subnet-b", "subnet-c"}
	f := &fakeEC2{runErrs: []error{sdkErr(500, "InsufficientInstanceCapacity"), sdkErr(400, "Unsupported")}, states: []string{"running"}}
	srv, err := (&AWS{EC2: f, RunningWait: time.Second}).Create(context.Background(), s)
	if err != nil || srv.ID != "i-0123456789abcdef0" || len(f.runs) != 3 {
		t.Fatalf("got %+v, %v after %d runs", srv, err, len(f.runs))
	}
	var tried []string
	for _, r := range f.runs {
		tried = append(tried, aws.ToString(r.NetworkInterfaces[0].SubnetId))
	}
	if slices.Sort(tried); !slices.Equal(tried, s.Subnets) {
		t.Errorf("tried %v", tried)
	}
	tokens := map[string]bool{}
	for _, r := range f.runs {
		tokens[aws.ToString(r.ClientToken)] = true
	}
	if len(tokens) != 3 || aws.ToString(f.runs[0].ClientToken) != s.Labels[Label+"-nonce"] {
		t.Errorf("client tokens %v", tokens)
	}
	// Every zone full: refused, so the caller forgets the entry.
	f = &fakeEC2{runErrs: []error{sdkErr(500, "InsufficientInstanceCapacity"), sdkErr(500, "InsufficientInstanceCapacity"), sdkErr(500, "InsufficientInstanceCapacity")}}
	if _, err := (&AWS{EC2: f, RunningWait: time.Second}).Create(context.Background(), s); !Refused(err) || len(f.runs) != 3 {
		t.Fatalf("all full: %v after %d runs", err, len(f.runs))
	}
	// Spread: workers don't all start in the same zone.
	firsts := map[string]bool{}
	for _, n := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd", "eeeeeeee", "ffffffff"} {
		s.Labels[Label+"-nonce"] = n
		f = &fakeEC2{states: []string{"running"}}
		(&AWS{EC2: f, RunningWait: time.Second}).Create(context.Background(), s)
		firsts[aws.ToString(f.runs[0].NetworkInterfaces[0].SubnetId)] = true
	}
	if len(firsts) < 2 {
		t.Errorf("every worker started in %v", firsts)
	}
	// Anything else, even a blip, is not a zone's problem: no next one.
	f = &fakeEC2{runErrs: []error{sdkErr(400, "RequestTimeout")}}
	if _, err := (&AWS{EC2: f, RunningWait: time.Second}).Create(context.Background(), s); err == nil || len(f.runs) != 1 {
		t.Fatalf("timeout: %v after %d runs", err, len(f.runs))
	}
	f = &fakeEC2{runErrs: []error{sdkErr(400, "VcpuLimitExceeded")}}
	if _, err := (&AWS{EC2: f, RunningWait: time.Second}).Create(context.Background(), s); err == nil || len(f.runs) != 1 {
		t.Fatalf("vcpu limit: %v after %d runs", err, len(f.runs))
	}
}

func (f *fakeEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, f.sgErr
}

func TestAWSDeletePoolSaysInUseWhileAnInstanceHoldsTheGroup(t *testing.T) {
	f := &fakeEC2{groups: []types.SecurityGroup{{GroupId: aws.String("sg-1")}}, sgErr: sdkErr(400, "DependencyViolation")}
	if err := (&AWS{EC2: f}).DeletePool(context.Background(), "m2test-p", ""); !InUse(err) {
		t.Fatalf("not in use: %v", err)
	}
}

func TestAWSDeletePoolAsksForOurTaggedGroupOnly(t *testing.T) {
	f := &fakeEC2{groups: []types.SecurityGroup{{GroupId: aws.String("sg-1")}}}
	if err := (&AWS{EC2: f}).DeletePool(context.Background(), "m2test-p", ""); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"group-name": "m2test-p", "tag:" + Label: "firewall"}
	got := map[string]string{}
	for _, fl := range f.groupsIn.Filters {
		got[aws.ToString(fl.Name)] = strings.Join(fl.Values, ",")
	}
	if !maps.Equal(got, want) || !slices.Equal(f.deleted, []string{"sg-1"}) {
		t.Fatalf("filters %v, deleted %v", got, f.deleted)
	}
}

func TestAWSGivesUpOnAnEndpointThatNeverAnswers(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	t.Cleanup(func() { close(hang); srv.Close() })
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	old := awsTimeout
	awsTimeout = 100 * time.Millisecond
	t.Cleanup(func() { awsTimeout = old })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := NewAWS(ctx, "eu-west-2"); err == nil || ctx.Err() != nil {
		t.Fatalf("still waiting when the context gave up: %v", err)
	}
}
