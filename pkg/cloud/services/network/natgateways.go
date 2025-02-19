/*
Copyright 2018 The Kubernetes Authors.

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

package network

import (
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/util/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/converters"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/filter"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/wait"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/tags"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/record"
)

func (s *Service) reconcileNatGateways() error {
	if s.scope.VPC().IsUnmanaged(s.scope.Name()) {
		s.scope.V(4).Info("Skipping NAT gateway reconcile in unmanaged mode")
		return nil
	}

	s.scope.V(2).Info("Reconciling NAT gateways")

	if len(s.scope.Subnets().FilterPrivate()) == 0 {
		s.scope.V(2).Info("No private subnets available, skipping NAT gateways")
		conditions.MarkFalse(
			s.scope.InfraCluster(),
			infrav1.NatGatewaysReadyCondition,
			infrav1.NatGatewaysReconciliationFailedReason,
			clusterv1.ConditionSeverityWarning,
			"No private subnets available, skipping NAT gateways")
		return nil
	} else if len(s.scope.Subnets().FilterPublic()) == 0 {
		s.scope.V(2).Info("No public subnets available. Cannot create NAT gateways for private subnets, this might be a configuration error.")
		conditions.MarkFalse(
			s.scope.InfraCluster(),
			infrav1.NatGatewaysReadyCondition,
			infrav1.NatGatewaysReconciliationFailedReason,
			clusterv1.ConditionSeverityWarning,
			"No public subnets available. Cannot create NAT gateways for private subnets, this might be a configuration error.")
		return nil
	}

	existing, err := s.describeNatGatewaysBySubnet()
	if err != nil {
		return err
	}

	subnetIDs := []string{}

	for _, sn := range s.scope.Subnets().FilterPublic() {
		if sn.ID == "" {
			continue
		}

		if ngw, ok := existing[sn.ID]; ok {
			// Make sure tags are up to date.
			if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
				buildParams := s.getNatGatewayTagParams(*ngw.NatGatewayId)
				tagsBuilder := tags.New(&buildParams, tags.WithEC2(s.EC2Client))
				if err := tagsBuilder.Ensure(converters.TagsToMap(ngw.Tags)); err != nil {
					return false, err
				}
				return true, nil
			}, awserrors.ResourceNotFound); err != nil {
				record.Warnf(s.scope.InfraCluster(), "FailedTagNATGateway", "Failed to tag managed NAT Gateway %q: %v", *ngw.NatGatewayId, err)
				return errors.Wrapf(err, "failed to tag nat gateway %q", *ngw.NatGatewayId)
			}

			continue
		}

		subnetIDs = append(subnetIDs, sn.ID)
	}

	// Batch the creation of NAT gateways
	if len(subnetIDs) > 0 {
		// set NatGatewayCreationStarted if the condition has never been set before
		if !conditions.Has(s.scope.InfraCluster(), infrav1.NatGatewaysReadyCondition) {
			conditions.MarkFalse(s.scope.InfraCluster(), infrav1.NatGatewaysReadyCondition, infrav1.NatGatewaysCreationStartedReason, clusterv1.ConditionSeverityInfo, "")
			if err := s.scope.PatchObject(); err != nil {
				return errors.Wrap(err, "failed to patch conditions")
			}
		}
		ngws, err := s.createNatGateways(subnetIDs)

		for _, ng := range ngws {
			subnet := s.scope.Subnets().FindByID(*ng.SubnetId)
			subnet.NatGatewayID = ng.NatGatewayId
		}

		if err != nil {
			return err
		}
		conditions.MarkTrue(s.scope.InfraCluster(), infrav1.NatGatewaysReadyCondition)
	}

	return nil
}

func (s *Service) deleteNatGateways() error {
	if s.scope.VPC().IsUnmanaged(s.scope.Name()) {
		s.scope.V(4).Info("Skipping NAT gateway deletion in unmanaged mode")
		return nil
	}

	if len(s.scope.Subnets().FilterPrivate()) == 0 {
		s.scope.V(2).Info("No private subnets available, skipping NAT gateways")
		return nil
	} else if len(s.scope.Subnets().FilterPublic()) == 0 {
		s.scope.V(2).Info("No public subnets available. Cannot create NAT gateways for private subnets, this might be a configuration error.")
		return nil
	}

	existing, err := s.describeNatGatewaysBySubnet()
	if err != nil {
		return err
	}

	var ngIDs []*ec2.NatGateway
	for _, sn := range s.scope.Subnets().FilterPublic() {
		if sn.ID == "" {
			continue
		}

		if ngID, ok := existing[sn.ID]; ok {
			ngIDs = append(ngIDs, ngID)
		}
	}

	c := make(chan error, len(ngIDs))
	errs := []error{}

	for _, ngID := range ngIDs {
		go func(c chan error, ngID *ec2.NatGateway) {
			err := s.deleteNatGateway(*ngID.NatGatewayId)
			c <- err
		}(c, ngID)
	}

	for i := 0; i < len(ngIDs); i++ {
		ngwErr := <-c
		if ngwErr != nil {
			errs = append(errs, ngwErr)
		}
	}

	return kerrors.NewAggregate(errs)
}

func (s *Service) describeNatGatewaysBySubnet() (map[string]*ec2.NatGateway, error) {
	describeNatGatewayInput := &ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{
			filter.EC2.VPC(s.scope.VPC().ID),
			filter.EC2.NATGatewayStates(ec2.NatGatewayStatePending, ec2.NatGatewayStateAvailable),
		},
	}

	gateways := make(map[string]*ec2.NatGateway)

	err := s.EC2Client.DescribeNatGatewaysPages(describeNatGatewayInput,
		func(page *ec2.DescribeNatGatewaysOutput, lastPage bool) bool {
			for _, r := range page.NatGateways {
				gateways[*r.SubnetId] = r
			}
			return !lastPage
		})
	if err != nil {
		record.Eventf(s.scope.InfraCluster(), "FailedDescribeNATGateways", "Failed to describe NAT gateways with VPC ID %q: %v", s.scope.VPC().ID, err)
		return nil, errors.Wrapf(err, "failed to describe NAT gateways with VPC ID %q", s.scope.VPC().ID)
	}

	return gateways, nil
}

func (s *Service) getNatGatewayTagParams(id string) infrav1.BuildParams {
	name := fmt.Sprintf("%s-nat", s.scope.Name())

	return infrav1.BuildParams{
		ClusterName: s.scope.Name(),
		ResourceID:  id,
		Lifecycle:   infrav1.ResourceLifecycleOwned,
		Name:        aws.String(name),
		Role:        aws.String(infrav1.CommonRoleTagValue),
		Additional:  s.scope.AdditionalTags(),
	}
}

func (s *Service) createNatGateways(subnetIDs []string) (natgateways []*ec2.NatGateway, err error) {
	eips, err := s.getOrAllocateAddresses(len(subnetIDs), infrav1.APIServerRoleTagValue)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create one or more IP addresses for NAT gateways")
	}
	type ngwCreation struct {
		natGateway *ec2.NatGateway
		error      error
	}
	c := make(chan ngwCreation, len(subnetIDs))

	for i, sn := range subnetIDs {
		go func(c chan ngwCreation, subnetID, ip string) {
			ngw, err := s.createNatGateway(subnetID, ip)
			c <- ngwCreation{natGateway: ngw, error: err}
		}(c, sn, eips[i])
	}

	for i := 0; i < len(subnetIDs); i++ {
		ngwResult := <-c
		if ngwResult.error != nil {
			return nil, err
		}
		natgateways = append(natgateways, ngwResult.natGateway)
	}
	return natgateways, nil
}

func (s *Service) createNatGateway(subnetID, ip string) (*ec2.NatGateway, error) {
	var out *ec2.CreateNatGatewayOutput
	var err error

	if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
		if out, err = s.EC2Client.CreateNatGateway(&ec2.CreateNatGatewayInput{
			SubnetId:          aws.String(subnetID),
			AllocationId:      aws.String(ip),
			TagSpecifications: []*ec2.TagSpecification{tags.BuildParamsToTagSpecification(ec2.ResourceTypeNatgateway, s.getNatGatewayTagParams(services.TemporaryResourceID))},
		}); err != nil {
			return false, err
		}
		return true, nil
	}, awserrors.InvalidSubnet); err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedCreateNATGateway", "Failed to create new NAT Gateway: %v", err)
		return nil, errors.Wrapf(err, "failed to create NAT gateway for subnet ID %q", subnetID)
	}
	record.Eventf(s.scope.InfraCluster(), "SuccessfulCreateNATGateway", "Created new NAT Gateway %q", *out.NatGateway.NatGatewayId)

	wReq := &ec2.DescribeNatGatewaysInput{NatGatewayIds: []*string{out.NatGateway.NatGatewayId}}
	if err := s.EC2Client.WaitUntilNatGatewayAvailable(wReq); err != nil {
		return nil, errors.Wrapf(err, "failed to wait for nat gateway %q in subnet %q", *out.NatGateway.NatGatewayId, subnetID)
	}

	s.scope.Info("NAT gateway for subnet is now available", "nat-gateway-id", *out.NatGateway.NatGatewayId, "subnet-id", subnetID)
	return out.NatGateway, nil
}

func (s *Service) deleteNatGateway(id string) error {
	_, err := s.EC2Client.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(id),
	})
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedDeleteNATGateway", "Failed to delete NAT Gateway %q previously attached to VPC %q: %v", id, s.scope.VPC().ID, err)
		return errors.Wrapf(err, "failed to delete nat gateway %q", id)
	}
	record.Eventf(s.scope.InfraCluster(), "SuccessfulDeleteNATGateway", "Deleted NAT Gateway %q previously attached to VPC %q", id, s.scope.VPC().ID)
	s.scope.Info("Deleted NAT gateway in VPC", "nat-gateway-id", id, "vpc-id", s.scope.VPC().ID)

	describeInput := &ec2.DescribeNatGatewaysInput{
		NatGatewayIds: []*string{aws.String(id)},
	}

	if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (done bool, err error) {
		out, err := s.EC2Client.DescribeNatGateways(describeInput)
		if err != nil {
			return false, err
		}

		if len(out.NatGateways) == 0 {
			return false, errors.Wrapf(err, "no NAT gateway returned for id %q", id)
		}

		ng := out.NatGateways[0]
		switch state := ng.State; *state {
		case ec2.NatGatewayStateAvailable, ec2.NatGatewayStateDeleting:
			return false, nil
		case ec2.NatGatewayStateDeleted:
			return true, nil
		case ec2.NatGatewayStatePending:
			return false, errors.Errorf("in pending state")
		case ec2.NatGatewayStateFailed:
			return false, errors.Errorf("in failed state: %q - %s", *ng.FailureCode, *ng.FailureMessage)
		}

		return false, errors.Errorf("in unknown state")
	}); err != nil {
		return errors.Wrapf(err, "failed to wait for NAT gateway deletion %q", id)
	}

	return nil
}

func (s *Service) getNatGatewayForSubnet(sn *infrav1.SubnetSpec) (string, error) {
	if sn.IsPublic {
		return "", errors.Errorf("cannot get NAT gateway for a public subnet, got id %q", sn.ID)
	}

	azGateways := make(map[string][]string)
	for _, psn := range s.scope.Subnets().FilterPublic() {
		if psn.NatGatewayID == nil {
			continue
		}

		azGateways[psn.AvailabilityZone] = append(azGateways[psn.AvailabilityZone], *psn.NatGatewayID)
	}

	if gws, ok := azGateways[sn.AvailabilityZone]; ok && len(gws) > 0 {
		return gws[0], nil
	}

	return "", errors.Errorf("no nat gateways available in %q for private subnet %q, current state: %+v", sn.AvailabilityZone, sn.ID, azGateways)
}
