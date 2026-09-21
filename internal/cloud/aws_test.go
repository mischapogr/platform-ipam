package cloud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/mischapogr/platform-ipam/internal/domain"
)

type fakeSTS struct {
	account     string
	assumeErr   error
	identityErr error
}

func (f *fakeSTS) AssumeRole(ctx context.Context, input *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	if f.assumeErr != nil {
		return nil, f.assumeErr
	}
	exp := time.Now().Add(time.Hour)
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String("key"), SecretAccessKey: aws.String("secret"), Expiration: &exp}}, nil
}
func (f *fakeSTS) GetCallerIdentity(ctx context.Context, input *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.identityErr != nil {
		return nil, f.identityErr
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

type fakeEC2 struct {
	vpcCalls, subnetCalls int
	vpcs                  []ec2types.Vpc
	subnets               []ec2types.Subnet
	zones                 []ec2types.AvailabilityZone
	failSubnet            bool
}

func (f *fakeEC2) DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	f.vpcCalls++
	if in.NextToken == nil {
		return &ec2.DescribeVpcsOutput{Vpcs: f.vpcs[:1], NextToken: aws.String("next")}, nil
	}
	return &ec2.DescribeVpcsOutput{Vpcs: f.vpcs[1:]}, nil
}
func (f *fakeEC2) DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	f.subnetCalls++
	if f.failSubnet {
		return nil, errors.New("denied page")
	}
	if in.NextToken == nil {
		return &ec2.DescribeSubnetsOutput{Subnets: f.subnets[:1], NextToken: aws.String("next")}, nil
	}
	return &ec2.DescribeSubnetsOutput{Subnets: f.subnets[1:]}, nil
}
func (f *fakeEC2) DescribeAvailabilityZones(ctx context.Context, in *ec2.DescribeAvailabilityZonesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
	return &ec2.DescribeAvailabilityZonesOutput{AvailabilityZones: f.zones}, nil
}

func awsFixture() (*fakeSTS, *fakeEC2, domain.Domain) {
	stsClient := &fakeSTS{account: "123456789012"}
	vpc := ec2types.Vpc{VpcId: aws.String("vpc-1"), CidrBlock: aws.String("10.0.0.0/16"), Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("one")}}, CidrBlockAssociationSet: []ec2types.VpcCidrBlockAssociation{{CidrBlock: aws.String("10.1.0.0/16"), CidrBlockState: &ec2types.VpcCidrBlockState{State: ec2types.VpcCidrBlockStateCodeAssociated}}}}
	subnet := ec2types.Subnet{SubnetId: aws.String("subnet-1"), VpcId: aws.String("vpc-1"), CidrBlock: aws.String("10.0.1.0/24"), AvailabilityZoneId: aws.String("euc1-az1")}
	ec := &fakeEC2{vpcs: []ec2types.Vpc{vpc, vpc}, subnets: []ec2types.Subnet{subnet, subnet}, zones: []ec2types.AvailabilityZone{{ZoneId: aws.String("euc1-az1"), State: ec2types.AvailabilityZoneStateAvailable}}}
	d := domain.Domain{ID: "d", CoverageGeneration: "generation-1", CloudCoverage: []domain.Cell{{AccountID: "123456789012", RoleARN: "arn:aws:iam::123456789012:role/read", Regions: []string{"eu-central-1"}}}}
	return stsClient, ec, d
}

func TestObserveAssumesRolePaginatesAndRetainsSecondaryCIDRAndUntagged(t *testing.T) {
	stsClient, ec, d := awsFixture()
	ec.vpcs[1].VpcId = aws.String("vpc-2")
	ec.subnets[1].SubnetId = aws.String("subnet-2")
	ec.vpcs[1].Tags = nil
	ec.subnets[1].Tags = nil
	c := NewAWS(Config{Factories: Factories{STS: func(aws.Config) STSAPI { return stsClient }, EC2: func(aws.Config) EC2API { return ec }}, Timeout: time.Second})
	obs, err := c.Observe(context.Background(), d)
	if err != nil || !obs.Complete {
		t.Fatalf("observation failed: complete=%v err=%v", obs.Complete, err)
	}
	if ec.vpcCalls != 2 || ec.subnetCalls != 2 {
		t.Fatalf("expected all pages, calls vpc=%d subnet=%d", ec.vpcCalls, ec.subnetCalls)
	}
	if len(obs.Resources) != 4 {
		t.Fatalf("expected 4 resources, got %d", len(obs.Resources))
	}
	if len(obs.Resources[0].CIDRs) != 2 {
		t.Fatalf("secondary CIDR was lost: %#v", obs.Resources[0].CIDRs)
	}
	if obs.Resources[2].Tags == nil || len(obs.Resources[2].Tags) != 0 {
		t.Fatalf("untagged occupancy should remain observable: %#v", obs.Resources[2].Tags)
	}
}

func TestObserveDeniedAssumptionIsUnknown(t *testing.T) {
	_, _, d := awsFixture()
	denied := &fakeSTS{assumeErr: errors.New("AccessDenied")}
	c := NewAWS(Config{Factories: Factories{STS: func(aws.Config) STSAPI { return denied }, EC2: func(aws.Config) EC2API { return &fakeEC2{} }}})
	obs, err := c.Observe(context.Background(), d)
	if err == nil || obs.Complete || obs.Error == "" {
		t.Fatalf("denied assumption must be UNKNOWN: %#v err=%v", obs, err)
	}
}

func TestObserveMismatchedAccountIsUnknown(t *testing.T) {
	_, _, d := awsFixture()
	wrong := &fakeSTS{account: "999999999999"}
	c := NewAWS(Config{Factories: Factories{STS: func(aws.Config) STSAPI { return wrong }, EC2: func(aws.Config) EC2API { return &fakeEC2{} }}})
	obs, err := c.Observe(context.Background(), d)
	if err == nil || obs.Complete {
		t.Fatalf("mismatched account must be UNKNOWN: %#v err=%v", obs, err)
	}
}

func TestObserveFailedLaterPageIsUnknown(t *testing.T) {
	stsClient, ec, d := awsFixture()
	ec.failSubnet = true
	c := NewAWS(Config{Factories: Factories{STS: func(aws.Config) STSAPI { return stsClient }, EC2: func(aws.Config) EC2API { return ec }}})
	obs, err := c.Observe(context.Background(), d)
	if err == nil || obs.Complete {
		t.Fatalf("failed page must be UNKNOWN: %#v err=%v", obs, err)
	}
}

func TestObserveRejectsEmptyAndDuplicateCoverage(t *testing.T) {
	stsClient, ec, d := awsFixture()
	c := NewAWS(Config{Factories: Factories{STS: func(aws.Config) STSAPI { return stsClient }, EC2: func(aws.Config) EC2API { return ec }}})
	empty := d
	empty.CloudCoverage = nil
	obs, err := c.Observe(context.Background(), empty)
	if err == nil || obs.Complete {
		t.Fatalf("empty coverage must be UNKNOWN: %#v err=%v", obs, err)
	}
	duplicate := d
	duplicate.CloudCoverage = append(duplicate.CloudCoverage, duplicate.CloudCoverage[0])
	obs, err = c.Observe(context.Background(), duplicate)
	if err == nil || obs.Complete {
		t.Fatalf("duplicate coverage must be UNKNOWN: %#v err=%v", obs, err)
	}
}
