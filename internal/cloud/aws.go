// Package cloud provides trusted AWS occupancy observations. It deliberately
// has no mutation methods: the observer can read VPC/subnet state only.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/mischapogr/platform-ipam/internal/domain"
)

// STSAPI and EC2API are the small SDK seams used by AWS. They match AWS SDK
// v2 clients, so production construction uses the official clients while
// tests can inject deterministic paginated responses.
type STSAPI interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}
type EC2API interface {
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeAvailabilityZones(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error)
}

type Factories struct {
	STS func(aws.Config) STSAPI
	EC2 func(aws.Config) EC2API
}

type Config struct {
	AWS         aws.Config
	Factories   Factories
	Timeout     time.Duration
	SessionName string
}

type Client struct{ cfg Config }

func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.SessionName == "" {
		cfg.SessionName = "platform-ipam-observer"
	}
	if cfg.Factories.STS == nil {
		cfg.Factories.STS = func(c aws.Config) STSAPI { return sts.NewFromConfig(c) }
	}
	if cfg.Factories.EC2 == nil {
		cfg.Factories.EC2 = func(c aws.Config) EC2API { return ec2.NewFromConfig(c) }
	}
	return &Client{cfg: cfg}
}

// NewAWS is the named convenience constructor used by wiring code.
func NewAWS(cfg Config) *Client { return New(cfg) }

func tags(in []ec2types.Tag) map[string]string {
	out := make(map[string]string, len(in))
	for _, t := range in {
		if t.Key != nil {
			out[*t.Key] = aws.ToString(t.Value)
		}
	}
	return out
}
func failedAssociation(v ec2types.Vpc) bool {
	for _, a := range v.CidrBlockAssociationSet {
		if a.CidrBlockState != nil && a.CidrBlockState.State != ec2types.VpcCidrBlockStateCodeAssociated {
			return true
		}
	}
	return false
}

func (c *Client) cell(ctx context.Context, d domain.Domain, cell domain.Cell) ([]domain.Resource, error) {
	if cell.AccountID == "" || cell.RoleARN == "" {
		return nil, errors.New("AWS coverage cell requires account_id and role_arn")
	}
	base := c.cfg.AWS
	stsBase := c.cfg.Factories.STS(base)
	assumeCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	out, err := stsBase.AssumeRole(assumeCtx, &sts.AssumeRoleInput{RoleArn: aws.String(cell.RoleARN), RoleSessionName: aws.String(c.cfg.SessionName)})
	if err != nil {
		return nil, fmt.Errorf("assume role %s: %w", cell.RoleARN, err)
	}
	if out == nil || out.Credentials == nil || out.Credentials.AccessKeyId == nil || out.Credentials.SecretAccessKey == nil {
		return nil, errors.New("assume role returned no credentials")
	}
	credentialValue := aws.Credentials{AccessKeyID: aws.ToString(out.Credentials.AccessKeyId), SecretAccessKey: aws.ToString(out.Credentials.SecretAccessKey), SessionToken: aws.ToString(out.Credentials.SessionToken)}
	if out.Credentials.Expiration != nil {
		credentialValue.CanExpire = true
		credentialValue.Expires = aws.ToTime(out.Credentials.Expiration)
	}
	creds := aws.NewCredentialsCache(&staticProvider{v: credentialValue})
	var resources []domain.Resource
	resourceIndex := make(map[string]int)
	for _, region := range cell.Regions {
		if region == "" {
			return nil, errors.New("AWS coverage cell has an empty region")
		}
		regionCtx, regionCancel := context.WithTimeout(ctx, c.cfg.Timeout)
		defer regionCancel()
		cfg := base
		cfg.Region = region
		cfg.Credentials = creds
		targetSTS := c.cfg.Factories.STS(cfg)
		id, err := targetSTS.GetCallerIdentity(regionCtx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return nil, fmt.Errorf("get caller identity for %s/%s: %w", cell.AccountID, region, err)
		}
		observedAccount := ""
		if id != nil {
			observedAccount = aws.ToString(id.Account)
		}
		if observedAccount != cell.AccountID {
			return nil, fmt.Errorf("AWS account mismatch for %s/%s: got %s", cell.AccountID, region, observedAccount)
		}
		ec := c.cfg.Factories.EC2(cfg)
		vpcs, err := allVPCs(regionCtx, ec)
		if err != nil {
			return nil, fmt.Errorf("describe VPCs for %s/%s: %w", cell.AccountID, region, err)
		}
		subnets, err := allSubnets(regionCtx, ec)
		if err != nil {
			return nil, fmt.Errorf("describe subnets for %s/%s: %w", cell.AccountID, region, err)
		}
		zones, err := allZones(regionCtx, ec)
		if err != nil {
			return nil, fmt.Errorf("describe availability zones for %s/%s: %w", cell.AccountID, region, err)
		}
		available := map[string]bool{}
		for _, z := range zones {
			if z.ZoneId != nil && z.State == ec2types.AvailabilityZoneStateAvailable {
				available[aws.ToString(z.ZoneId)] = true
			}
		}
		for _, v := range vpcs {
			if v.VpcId == nil || v.CidrBlock == nil {
				return nil, errors.New("AWS VPC response omitted identity or primary CIDR")
			}
			if failedAssociation(v) {
				return nil, fmt.Errorf("VPC %s has an unresolved CIDR association", aws.ToString(v.VpcId))
			}
			r := domain.Resource{AccountID: cell.AccountID, Region: region, Type: "vpc", ID: aws.ToString(v.VpcId), CIDR: aws.ToString(v.CidrBlock), Tags: tags(v.Tags), State: string(v.State)}
			r.CIDRs = append(r.CIDRs, r.CIDR)
			for _, a := range v.CidrBlockAssociationSet {
				if a.CidrBlock != nil {
					r.CIDRs = append(r.CIDRs, aws.ToString(a.CidrBlock))
				}
			}
			key := "vpc\x00" + r.ID
			if i, ok := resourceIndex[key]; ok {
				if resources[i].CIDR != r.CIDR {
					return nil, fmt.Errorf("duplicate VPC %s has conflicting primary CIDRs", r.ID)
				}
				resources[i].CIDRs = mergeStrings(resources[i].CIDRs, r.CIDRs)
			} else {
				resourceIndex[key] = len(resources)
				resources = append(resources, r)
			}
		}
		for _, s := range subnets {
			if s.SubnetId == nil || s.CidrBlock == nil || s.VpcId == nil {
				return nil, errors.New("AWS subnet response omitted identity, CIDR, or parent")
			}
			if s.AvailabilityZoneId == nil || aws.ToString(s.AvailabilityZoneId) == "" {
				return nil, fmt.Errorf("subnet %s omitted availability zone ID", aws.ToString(s.SubnetId))
			}
			zone := aws.ToString(s.AvailabilityZoneId)
			if zone != "" && !available[zone] {
				return nil, fmt.Errorf("subnet %s AZ %s is not available in target account/region", aws.ToString(s.SubnetId), zone)
			}
			r := domain.Resource{AccountID: cell.AccountID, Region: region, Type: "subnet", ID: aws.ToString(s.SubnetId), CIDR: aws.ToString(s.CidrBlock), CIDRs: []string{aws.ToString(s.CidrBlock)}, ParentID: aws.ToString(s.VpcId), ZoneID: zone, Tags: tags(s.Tags), State: string(s.State)}
			key := "subnet\x00" + r.ID
			if i, ok := resourceIndex[key]; ok {
				if resources[i].CIDR != r.CIDR || resources[i].ParentID != r.ParentID || resources[i].ZoneID != r.ZoneID {
					return nil, fmt.Errorf("duplicate subnet %s has conflicting identity", r.ID)
				}
			} else {
				resourceIndex[key] = len(resources)
				resources = append(resources, r)
			}
		}
	}
	return resources, nil
}

func mergeStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, x := range append(append([]string(nil), a...), b...) {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

type staticProvider struct{ v aws.Credentials }

func (s *staticProvider) Retrieve(context.Context) (aws.Credentials, error) { return s.v, nil }

func allVPCs(ctx context.Context, c EC2API) ([]ec2types.Vpc, error) {
	var out []ec2types.Vpc
	var token *string
	seen := map[string]bool{}
	for {
		r, e := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{NextToken: token})
		if e != nil {
			return nil, e
		}
		if r == nil {
			return nil, errors.New("nil DescribeVpcs response")
		}
		out = append(out, r.Vpcs...)
		if r.NextToken == nil || aws.ToString(r.NextToken) == "" {
			return out, nil
		}
		if seen[aws.ToString(r.NextToken)] {
			return nil, errors.New("DescribeVpcs pagination token repeated")
		}
		seen[aws.ToString(r.NextToken)] = true
		token = r.NextToken
	}
}
func allSubnets(ctx context.Context, c EC2API) ([]ec2types.Subnet, error) {
	var out []ec2types.Subnet
	var token *string
	seen := map[string]bool{}
	for {
		r, e := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{NextToken: token})
		if e != nil {
			return nil, e
		}
		if r == nil {
			return nil, errors.New("nil DescribeSubnets response")
		}
		out = append(out, r.Subnets...)
		if r.NextToken == nil || aws.ToString(r.NextToken) == "" {
			return out, nil
		}
		if seen[aws.ToString(r.NextToken)] {
			return nil, errors.New("DescribeSubnets pagination token repeated")
		}
		seen[aws.ToString(r.NextToken)] = true
		token = r.NextToken
	}
}
func allZones(ctx context.Context, c EC2API) ([]ec2types.AvailabilityZone, error) {
	r, e := c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	if e != nil {
		return nil, e
	}
	if r == nil {
		return nil, errors.New("nil DescribeAvailabilityZones response")
	}
	return r.AvailabilityZones, nil
}

func (c *Client) Observe(ctx context.Context, d domain.Domain) (domain.Observation, error) {
	started := time.Now().UTC()
	obs := domain.Observation{DomainID: d.ID, Generation: d.CoverageGeneration, StartedAt: started}
	if d.ID == "" || d.CoverageGeneration == "" {
		obs.FinishedAt = time.Now().UTC()
		obs.Error = "domain ID and coverage generation are required"
		return obs, errors.New(obs.Error)
	}
	if len(d.CloudCoverage) == 0 {
		obs.FinishedAt = time.Now().UTC()
		obs.Error = "AWS coverage is empty"
		return obs, errors.New(obs.Error)
	}
	seenCells := make(map[string]bool)
	for _, cell := range d.CloudCoverage {
		if cell.AccountID == "" || cell.RoleARN == "" || len(cell.Regions) == 0 {
			obs.FinishedAt = time.Now().UTC()
			obs.Error = "AWS coverage cell is incomplete"
			return obs, errors.New(obs.Error)
		}
		for _, region := range cell.Regions {
			key := cell.AccountID + "\x00" + region
			if seenCells[key] {
				obs.FinishedAt = time.Now().UTC()
				obs.Error = "AWS coverage contains a duplicate account/region cell"
				return obs, errors.New(obs.Error)
			}
			seenCells[key] = true
		}
	}
	for _, cell := range d.CloudCoverage {
		resources, err := c.cell(ctx, d, cell)
		if err != nil {
			obs.Complete = false
			obs.Error = err.Error()
			obs.FinishedAt = time.Now().UTC()
			obs.Resources = append(obs.Resources, resources...)
			return obs, err
		}
		obs.Resources = append(obs.Resources, resources...)
	}
	obs.Complete = true
	obs.FinishedAt = time.Now().UTC()
	return obs, nil
}

var _ domain.Observer = (*Client)(nil)
