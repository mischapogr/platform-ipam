package cloud

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/mischapogr/platform-ipam/internal/domain"
)

// This opt-in test uses the real AWS SDK request path against the isolated
// Moto Compose service. It does not establish IAM policy or AWS fidelity.
func TestMotoEC2STSProtocol(t *testing.T) {
	endpoint := os.Getenv("IPAM_TEST_MOTO_URL")
	if endpoint == "" {
		t.Skip("IPAM_TEST_MOTO_URL is required for the isolated Moto protocol test")
	}
	t.Setenv("AWS_ENDPOINT_URL", endpoint)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	base, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("eu-central-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("moto", "moto", "")))
	if err != nil {
		t.Fatal(err)
	}
	const account = "111111111111"
	role := "arn:aws:iam::" + account + ":role/platform-ipam-read"
	assumed, err := sts.NewFromConfig(base).AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: aws.String(role), RoleSessionName: aws.String("platform-ipam-moto-test"),
	})
	if err != nil {
		t.Fatalf("Moto STS assume role: %v", err)
	}
	accountCfg := base
	accountCfg.Credentials = aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
		aws.ToString(assumed.Credentials.AccessKeyId), aws.ToString(assumed.Credentials.SecretAccessKey), aws.ToString(assumed.Credentials.SessionToken)))
	ec := ec2.NewFromConfig(accountCfg)
	zones, err := ec.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	if err != nil {
		t.Fatalf("Moto availability zones: %v", err)
	}
	if len(zones.AvailabilityZones) == 0 || zones.AvailabilityZones[0].ZoneId == nil {
		t.Fatalf("Moto availability zones: count=%d err=%v", len(zones.AvailabilityZones), err)
	}
	vpc, err := ec.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.67.0.0/16")})
	if err != nil {
		t.Fatalf("Moto create VPC: %v", err)
	}
	subnet, err := ec.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId: vpc.Vpc.VpcId, CidrBlock: aws.String("10.67.1.0/24"),
		AvailabilityZone: zones.AvailabilityZones[0].ZoneName,
	})
	if err != nil {
		t.Fatalf("Moto create subnet: %v", err)
	}
	d := domain.Domain{ID: "moto-protocol", CoverageGeneration: "moto-run",
		CloudCoverage: []domain.Cell{{AccountID: account, RoleARN: role, Regions: []string{"eu-central-1"}}}}
	observer := NewAWS(Config{AWS: base})
	observation, err := observer.Observe(ctx, d)
	if err != nil || !observation.Complete {
		t.Fatalf("AWS SDK observation incomplete: err=%v observation=%#v", err, observation)
	}
	seenVPC, seenSubnet := false, false
	for _, resource := range observation.Resources {
		if resource.ID == aws.ToString(vpc.Vpc.VpcId) && resource.CIDR == "10.67.0.0/16" {
			seenVPC = true
		}
		if resource.ID == aws.ToString(subnet.Subnet.SubnetId) && resource.ParentID == aws.ToString(vpc.Vpc.VpcId) && resource.CIDR == "10.67.1.0/24" {
			seenSubnet = true
		}
	}
	if !seenVPC || !seenSubnet {
		t.Fatalf("VPC/subnet missing from SDK observation: vpc=%v subnet=%v", seenVPC, seenSubnet)
	}
	wrong := d
	wrong.CloudCoverage = []domain.Cell{{AccountID: "222222222222", RoleARN: role, Regions: []string{"eu-central-1"}}}
	refused, err := observer.Observe(ctx, wrong)
	if err == nil || refused.Complete {
		t.Fatalf("account mismatch did not make coverage UNKNOWN: err=%v observation=%#v", err, refused)
	}
}
