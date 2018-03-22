package broker

import (
	"context"
	"encoding/json"
	"fmt"

	"code.cloudfoundry.org/lager"
	"github.com/cf-platform-eng/kibosh/config"
	my_helm "github.com/cf-platform-eng/kibosh/helm"
	"github.com/cf-platform-eng/kibosh/k8s"
	"github.com/pivotal-cf/brokerapi"
	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/helm/pkg/helm"
	hapi_release "k8s.io/helm/pkg/proto/hapi/release"
	"strings"
)

const registrySecretName = "registry-secret"

// PksServiceBroker contains values passed in from configuration necessary for broker's work.
type PksServiceBroker struct {
	Logger         lager.Logger
	ServiceID      string
	registryConfig *config.RegistryConfig

	cluster      k8s.Cluster
	myHelmClient my_helm.MyHelmClient
	myChart      *my_helm.MyChart
}

func NewPksServiceBroker(serviceID string, registryConfig *config.RegistryConfig, cluster k8s.Cluster, myHelmClient my_helm.MyHelmClient, myChart *my_helm.MyChart, logger lager.Logger) *PksServiceBroker {
	return &PksServiceBroker{
		Logger:         logger,
		ServiceID:      serviceID,
		registryConfig: registryConfig,

		cluster:      cluster,
		myHelmClient: myHelmClient,
		myChart:      myChart,
	}
}

func (broker *PksServiceBroker) Services(ctx context.Context) []brokerapi.Service {

	plans := []brokerapi.ServicePlan{}

	for _, plan := range broker.myChart.Plans {

		plans = append(plans, brokerapi.ServicePlan{
			ID:          broker.ServiceID + "-" + plan.Name,
			Name:        plan.Name,
			Description: plan.Description,
		})

	}

	serviceCatalog := []brokerapi.Service{{
		ID:          broker.ServiceID,
		Name:        broker.myChart.Metadata.Name,
		Description: broker.myChart.Metadata.Description,
		Bindable:    true,

		Plans: plans,
	}}

	return serviceCatalog
}

func (broker *PksServiceBroker) Provision(ctx context.Context, instanceID string, details brokerapi.ProvisionDetails, asyncAllowed bool) (brokerapi.ProvisionedServiceSpec, error) {
	if !asyncAllowed {
		return brokerapi.ProvisionedServiceSpec{}, brokerapi.ErrAsyncRequired
	}

	namespaceName := broker.getNamespace(instanceID)
	namespace := api_v1.Namespace{
		Spec: api_v1.NamespaceSpec{},
		ObjectMeta: meta_v1.ObjectMeta{
			Name: namespaceName,
			Labels: map[string]string{
				"serviceID":        details.ServiceID,
				"planID":           details.PlanID,
				"organizationGUID": details.OrganizationGUID,
				"spaceGUID":        details.SpaceGUID,
			},
		},
	}
	_, err := broker.cluster.CreateNamespace(&namespace)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}

	if broker.registryConfig.HasRegistryConfig() {
		dockerConfig, _ := broker.registryConfig.GetDockerConfigJson()
		secret := &api_v1.Secret{
			ObjectMeta: meta_v1.ObjectMeta{
				Name: registrySecretName,
			},
			Type: api_v1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				api_v1.DockerConfigJsonKey: dockerConfig,
			},
		}
		_, err = broker.cluster.CreateSecret(namespaceName, secret)
		if err != nil {
			return brokerapi.ProvisionedServiceSpec{}, err
		}

		patch := map[string]interface{}{
			"imagePullSecrets": []map[string]interface{}{
				{"name": registrySecretName},
			},
		}
		patchJson, _ := json.Marshal(patch)
		broker.cluster.Patch(namespaceName, "default", types.MergePatchType, patchJson)
	}

	planName := strings.TrimPrefix(details.PlanID, details.ServiceID + "-")
	_, err = broker.myHelmClient.InstallChart(namespaceName, planName, helm.ReleaseName(namespaceName))
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}

	return brokerapi.ProvisionedServiceSpec{
		IsAsync:       true,
		OperationData: "provision",
	}, nil
}

func (broker *PksServiceBroker) Deprovision(ctx context.Context, instanceID string, details brokerapi.DeprovisionDetails, asyncAllowed bool) (brokerapi.DeprovisionServiceSpec, error) {
	_, err := broker.myHelmClient.DeleteRelease(broker.getNamespace(instanceID))
	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, err
	}

	err = broker.cluster.DeleteNamespace(broker.getNamespace(instanceID), &meta_v1.DeleteOptions{})
	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, err
	}

	return brokerapi.DeprovisionServiceSpec{
		IsAsync:       true,
		OperationData: "deprovision",
	}, nil
}

func (broker *PksServiceBroker) Bind(ctx context.Context, instanceID, bindingID string, details brokerapi.BindDetails) (brokerapi.Binding, error) {
	secrets, err := broker.cluster.ListSecrets(broker.getNamespace(instanceID), meta_v1.ListOptions{})
	if err != nil {
		return brokerapi.Binding{}, err
	}

	secretsMap := []map[string]interface{}{}
	for _, secret := range secrets.Items {
		if secret.Type == api_v1.SecretTypeOpaque {
			credentialSecrets := map[string]string{}
			for key, val := range secret.Data {
				credentialSecrets[key] = string(val)
			}
			credential := map[string]interface{}{
				"name": secret.Name,
				"data": credentialSecrets,
			}
			secretsMap = append(secretsMap, credential)
		}
	}

	services, err := broker.cluster.ListServices(broker.getNamespace(instanceID), meta_v1.ListOptions{})
	if err != nil {
		return brokerapi.Binding{}, err
	}

	servicesMap := []map[string]interface{}{}
	for _, service := range services.Items {
		credentialService := map[string]interface{}{
			"name":   service.ObjectMeta.Name,
			"spec":   service.Spec,
			"status": service.Status,
		}
		servicesMap = append(servicesMap, credentialService)
	}

	return brokerapi.Binding{
		Credentials: map[string]interface{}{
			"secrets":  secretsMap,
			"services": servicesMap,
		},
	}, nil
}

// Unbind reverses bind
func (broker *PksServiceBroker) Unbind(ctx context.Context, instanceID, bindingID string, details brokerapi.UnbindDetails) error {
	// noop

	return nil
}

// Update is perhaps not needed for MVP.
// Its purpose may be for changing plans, so if we only have a single default plan
// it is out of scope.
func (broker *PksServiceBroker) Update(ctx context.Context, instanceID string, details brokerapi.UpdateDetails, asyncAllowed bool) (brokerapi.UpdateServiceSpec, error) {
	return brokerapi.UpdateServiceSpec{}, nil
}

// LastOperation is for async
func (broker *PksServiceBroker) LastOperation(ctx context.Context, instanceID, operationData string) (brokerapi.LastOperation, error) {
	var brokerStatus brokerapi.LastOperationState
	var description string
	response, err := broker.myHelmClient.ReleaseStatus(broker.getNamespace(instanceID))
	if err != nil {
		return brokerapi.LastOperation{}, err
	}

	code := response.Info.Status.Code
	if operationData == "provision" {
		switch code {
		case hapi_release.Status_DEPLOYED:
			brokerStatus = brokerapi.Succeeded
			description = "service deployment succeeded"
		case hapi_release.Status_PENDING_INSTALL:
			fallthrough
		case hapi_release.Status_PENDING_UPGRADE:
			brokerStatus = brokerapi.InProgress
			description = "deploy in progress"
		default:
			brokerStatus = brokerapi.Failed
			description = fmt.Sprintf("provision failed %v", code)
		}
	} else if operationData == "deprovision" {
		switch code {

		case hapi_release.Status_DELETED:
			brokerStatus = brokerapi.Succeeded
			description = "gone"
		case hapi_release.Status_DEPLOYED:
			fallthrough
		case hapi_release.Status_DELETING:
			brokerStatus = brokerapi.InProgress
			description = "delete in progress"
		default:
			brokerStatus = brokerapi.Failed
			description = fmt.Sprintf("deprovision failed %v", code)
		}
	}

	services, err := broker.cluster.ListServices(broker.getNamespace(instanceID), meta_v1.ListOptions{})
	if err != nil {
		return brokerapi.LastOperation{}, err
	}

	serviceReady := true
	for _, service := range services.Items {
		if service.Spec.Type == "LoadBalancer" {
			if len(service.Status.LoadBalancer.Ingress) < 1 {
				serviceReady = false
			}
		}
	}
	if brokerStatus == brokerapi.Succeeded && !serviceReady {
		brokerStatus = brokerapi.InProgress
		description = "service deployment in progress"
	}

	return brokerapi.LastOperation{
		State:       brokerStatus,
		Description: description,
	}, nil
}

func (broker *PksServiceBroker) getNamespace(instanceID string) string {
	return "kibosh-" + instanceID
}
