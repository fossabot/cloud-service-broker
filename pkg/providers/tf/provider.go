// Copyright 2018 the Service Broker Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tf

import (
	"context"
	"fmt"

	"code.cloudfoundry.org/lager"
	"github.com/pivotal-cf/brokerapi"
	"github.com/pivotal/cloud-service-broker/db_service/models"
	"github.com/pivotal/cloud-service-broker/pkg/broker"
	"github.com/pivotal/cloud-service-broker/pkg/providers/builtin/base"
	"github.com/pivotal/cloud-service-broker/pkg/providers/tf/wrapper"
	"github.com/pivotal/cloud-service-broker/pkg/varcontext"
)

// NewTerraformProvider creates a new ServiceProvider backed by Terraform module definitions for provision and bind.
func NewTerraformProvider(jobRunner *TfJobRunner, logger lager.Logger, serviceDefinition TfServiceDefinitionV1) broker.ServiceProvider {
	return &terraformProvider{
		serviceDefinition: serviceDefinition,
		jobRunner:         jobRunner,
		logger:            logger.Session("terraform-" + serviceDefinition.Name),
	}
}

type terraformProvider struct {
	base.MergedInstanceCredsMixin

	logger            lager.Logger
	jobRunner         *TfJobRunner
	serviceDefinition TfServiceDefinitionV1
}

// Provision creates the necessary resources that an instance of this service
// needs to operate.
func (provider *terraformProvider) Provision(ctx context.Context, provisionContext *varcontext.VarContext) (models.ServiceInstanceDetails, error) {
	provider.logger.Info("provision", lager.Data{
		"context": provisionContext.ToMap(),
	})

	var tfID string
	var err error

	if provider.serviceDefinition.ProvisionSettings.IsTfImport() { 
		tfID, err = provider.importCreate(ctx, provisionContext, provider.serviceDefinition.ProvisionSettings)
		if err != nil {
			return models.ServiceInstanceDetails{}, err
		}	
	} else {
		tfID, err = provider.create(ctx, provisionContext, provider.serviceDefinition.ProvisionSettings)
		if err != nil {
			return models.ServiceInstanceDetails{}, err
		}
	}

	return models.ServiceInstanceDetails{
		OperationId:   tfID,
		OperationType: models.ProvisionOperationType,
	}, nil
}

// Update makes necessary updates to resources so they match new desired configuration
func (provider *terraformProvider) Update(ctx context.Context, provisionContext *varcontext.VarContext) (models.ServiceInstanceDetails, error) {
	provider.logger.Info("update", lager.Data{
		"context": provisionContext.ToMap(),
	})

	tfId := provisionContext.GetString("tf_id")
	if err := provisionContext.Error(); err != nil {
		return models.ServiceInstanceDetails{}, err
	}

	err :=  provider.jobRunner.Update(ctx, tfId, provisionContext.ToMap())

	return models.ServiceInstanceDetails{
		OperationId:   tfId,
		OperationType: models.UpdateOperationType,
	}, err
}

// Bind creates a new backing Terraform job and executes it, waiting on the result.
func (provider *terraformProvider) Bind(ctx context.Context, bindContext *varcontext.VarContext) (map[string]interface{}, error) {
	provider.logger.Info("bind", lager.Data{
		"context": bindContext.ToMap(),
	})

	tfId, err := provider.create(ctx, bindContext, provider.serviceDefinition.BindSettings)
	if err != nil {
		return nil, err
	}

	if err := provider.jobRunner.Wait(ctx, tfId); err != nil {
		return nil, err
	}

	return provider.jobRunner.Outputs(ctx, tfId, wrapper.DefaultInstanceName)
}

func (provider *terraformProvider) importCreate(ctx context.Context, vars *varcontext.VarContext, action TfServiceDefinitionV1Action) (string, error) {
	tfId := vars.GetString("tf_id")
	if err := vars.Error(); err != nil {
		return "", err
	}

	varsMap := vars.ToMap()

	var parameterMappings []wrapper.ParameterMapping

	for _, importParameterMapping := range action.ImportParameterMappings {
		parameterMappings = append(parameterMappings, wrapper.ParameterMapping{
			TfVariable: importParameterMapping.TfVariable,
			ParameterName: importParameterMapping.ParameterName,
		})
	}

	workspace, err := wrapper.NewWorkspace(varsMap, "", action.Templates, parameterMappings, action.ImportParametersToDelete)
	if err != nil {
		return tfId, err
	}

	if err := provider.jobRunner.StageJob(ctx, tfId, workspace); err != nil {
		provider.logger.Error("terraform provider create failed", err)
		return tfId, err
	}

	var importParams []ImportResource

	for _, importParam := range action.ImportVariables {
		param, ok := varsMap[importParam.Name]

		if !ok {
			return tfId, fmt.Errorf("Missing required import variable %v", importParam.Name)
		}
		importParams = append(importParams, ImportResource{TfResource: importParam.TfResource, IaaSResource: fmt.Sprintf("%v", param)})
	}

	return tfId, provider.jobRunner.Import(ctx, tfId, importParams)
}

func (provider *terraformProvider) create(ctx context.Context, vars *varcontext.VarContext, action TfServiceDefinitionV1Action) (string, error) {
	tfId := vars.GetString("tf_id")
	if err := vars.Error(); err != nil {
		return "", err
	}

	workspace, err := wrapper.NewWorkspace(vars.ToMap(), action.Template, map[string]string{}, []wrapper.ParameterMapping{}, []string{})
	if err != nil {
		return tfId, err
	}

	// if err = workspace.Validate(); err != nil {
	// 	return tfId, err
	// }

	if err := provider.jobRunner.StageJob(ctx, tfId, workspace); err != nil {
		provider.logger.Error("terraform provider create failed", err)
		return tfId, err
	}

	return tfId, provider.jobRunner.Create(ctx, tfId)
}

// Unbind performs a terraform destroy on the binding.
func (provider *terraformProvider) Unbind(ctx context.Context, instanceRecord models.ServiceInstanceDetails, bindRecord models.ServiceBindingCredentials) error {
	tfId := generateTfId(instanceRecord.ID, bindRecord.BindingId)
	provider.logger.Info("unbind", lager.Data{
		"instance": instanceRecord.ID,
		"binding":  bindRecord.ID,
		"tfId":     tfId,
	})

	if err := provider.jobRunner.Destroy(ctx, tfId); err != nil {
		return err
	}

	return provider.jobRunner.Wait(ctx, tfId)
}

// Deprovision performs a terraform destroy on the instance.
func (provider *terraformProvider) Deprovision(ctx context.Context, instance models.ServiceInstanceDetails, details brokerapi.DeprovisionDetails) (operationId *string, err error) {
	provider.logger.Info("terraform-deprovision", lager.Data{
		"instance": instance.ID,
	})

	tfId := generateTfId(instance.ID, "")
	if err := provider.jobRunner.Destroy(ctx, tfId); err != nil {
		return nil, err
	}

	return &tfId, nil
}

// PollInstance returns the instance status of the backing job.
func (provider *terraformProvider) PollInstance(ctx context.Context, instance models.ServiceInstanceDetails) (bool, string, error) {
	return provider.jobRunner.Status(ctx, generateTfId(instance.ID, ""))
}

// ProvisionsAsync is always true for Terraformprovider.
func (provider *terraformProvider) ProvisionsAsync() bool {
	return true
}

// DeprovisionsAsync is always true for Terraformprovider.
func (provider *terraformProvider) DeprovisionsAsync() bool {
	return true
}

// UpdateInstanceDetails updates the ServiceInstanceDetails with the most recent state from GCP.
// This function is optional, but will be called after async provisions, updates, and possibly
// on broker version changes.
// Return a nil error if you choose not to implement this function.
func (provider *terraformProvider) UpdateInstanceDetails(ctx context.Context, instance *models.ServiceInstanceDetails) error {
	tfId := generateTfId(instance.ID, "")

	outs, err := provider.jobRunner.Outputs(ctx, tfId, wrapper.DefaultInstanceName)
	if err != nil {
		return err
	}

	return instance.SetOtherDetails(outs)
}
