// Copyright (c) 2020-2021 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/common"
	"github.com/tigera/operator/pkg/controller/compliance"
	"github.com/tigera/operator/pkg/controller/options"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/controller/utils"
	"github.com/tigera/operator/pkg/controller/utils/imageset"
	"github.com/tigera/operator/pkg/dns"
	"github.com/tigera/operator/pkg/render"
	tigerakvc "github.com/tigera/operator/pkg/render/common/authentication/tigera/key_validator_config"
	relasticsearch "github.com/tigera/operator/pkg/render/common/elasticsearch"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_manager")

// Add creates a new Manager Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, opts options.AddOptions) error {
	if !opts.EnterpriseCRDExists {
		// No need to start this controller.
		return nil
	}

	var licenseAPIReady = &utils.ReadyFlag{}

	// create the reconciler
	reconciler := newReconciler(mgr, opts, licenseAPIReady)

	// Create a new controller
	controller, err := controller.New("cmanager-controller", mgr, controller.Options{Reconciler: reconcile.Reconciler(reconciler)})
	if err != nil {
		return fmt.Errorf("failed to create manager-controller: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Error(err, "Failed to establish a connection to k8s")
		return err
	}

	go utils.WaitToAddLicenseKeyWatch(controller, k8sClient, log, licenseAPIReady)

	return add(mgr, controller)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, opts options.AddOptions, licenseAPIReady *utils.ReadyFlag) reconcile.Reconciler {
	c := &ReconcileManager{
		client:          mgr.GetClient(),
		scheme:          mgr.GetScheme(),
		provider:        opts.DetectedProvider,
		status:          status.New(mgr.GetClient(), "manager", opts.KubernetesVersion),
		clusterDomain:   opts.ClusterDomain,
		licenseAPIReady: licenseAPIReady,
	}
	c.status.Run(opts.ShutdownContext)
	return c

}

// add adds watches for resources that are available at startup
func add(mgr manager.Manager, c controller.Controller) error {
	var err error

	// Watch for changes to primary resource Manager
	err = c.Watch(&source.Kind{Type: &operatorv1.Manager{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("manager-controller failed to watch primary resource: %w", err)
	}

	err = utils.AddAPIServerWatch(c)
	if err != nil {
		return fmt.Errorf("manager-controller failed to watch APIServer resource: %w", err)
	}

	err = utils.AddComplianceWatch(c)
	if err != nil {
		return fmt.Errorf("manager-controller failed to watch compliance resource: %w", err)
	}

	// Watch the given secrets in each both the manager and operator namespaces
	for _, namespace := range []string{common.OperatorNamespace(), render.ManagerNamespace} {
		for _, secretName := range []string{
			render.ManagerTLSSecretName, relasticsearch.PublicCertSecret,
			render.ElasticsearchManagerUserSecret, render.KibanaPublicCertSecret,
			render.VoltronTunnelSecretName, render.ComplianceServerCertSecret, render.PacketCaptureCertSecret,
			render.ManagerInternalTLSSecretName, render.DexCertSecretName, render.PrometheusTLSSecretName,
		} {
			if err = utils.AddSecretsWatch(c, secretName, namespace); err != nil {
				return fmt.Errorf("manager-controller failed to watch the secret '%s' in '%s' namespace: %w", secretName, namespace, err)
			}
		}
	}

	// This may or may not exist, it depends on what the OIDC type is in the Authentication CR.
	if err = utils.AddConfigMapWatch(c, tigerakvc.StaticWellKnownJWKSConfigMapName, common.OperatorNamespace()); err != nil {
		return fmt.Errorf("manager-controller failed to watch ConfigMap resource %s: %w", tigerakvc.StaticWellKnownJWKSConfigMapName, err)
	}

	if err = utils.AddConfigMapWatch(c, relasticsearch.ClusterConfigConfigMapName, common.OperatorNamespace()); err != nil {
		return fmt.Errorf("compliance-controller failed to watch the ConfigMap resource: %w", err)
	}

	if err = utils.AddNetworkWatch(c); err != nil {
		return fmt.Errorf("manager-controller failed to watch Network resource: %w", err)
	}

	if err = imageset.AddImageSetWatch(c); err != nil {
		return fmt.Errorf("manager-controller failed to watch ImageSet: %w", err)
	}

	if err = utils.AddNamespaceWatch(c, common.TigeraPrometheusNamespace); err != nil {
		return fmt.Errorf("manager-controller failed to watch the '%s' namespace: %w", common.TigeraPrometheusNamespace, err)
	}

	// Watch for changes to primary resource ManagementCluster
	err = c.Watch(&source.Kind{Type: &operatorv1.ManagementCluster{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("manager-controller failed to watch primary resource: %w", err)
	}

	// Watch for changes to primary resource ManagementClusterConnection
	err = c.Watch(&source.Kind{Type: &operatorv1.ManagementClusterConnection{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("manager-controller failed to watch primary resource: %w", err)
	}

	err = c.Watch(&source.Kind{Type: &operatorv1.Authentication{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("manager-controller failed to watch resource: %w", err)
	}

	if err = utils.AddConfigMapWatch(c, render.ECKLicenseConfigMapName, render.ECKOperatorNamespace); err != nil {
		return fmt.Errorf("manager-controller failed to watch the ConfigMap resource: %v", err)
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileManager{}

// ReconcileManager reconciles a Manager object
type ReconcileManager struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client          client.Client
	scheme          *runtime.Scheme
	provider        operatorv1.Provider
	status          status.StatusManager
	clusterDomain   string
	licenseAPIReady *utils.ReadyFlag
}

// GetManager returns the default manager instance with defaults populated.
func GetManager(ctx context.Context, cli client.Client) (*operatorv1.Manager, error) {
	// Fetch the manager instance. We only support a single instance named "tigera-secure".
	instance := &operatorv1.Manager{}
	err := cli.Get(ctx, utils.DefaultTSEEInstanceKey, instance)
	if err != nil {
		return nil, err
	}
	if instance.Spec.Auth != nil && instance.Spec.Auth.Type != operatorv1.AuthTypeToken {
		return nil, fmt.Errorf("auth types other than 'Token' can no longer be configured using the Manager CR, " +
			"please use the Authentication CR instead")
	}
	return instance, nil
}

// Reconcile reads that state of the cluster for a Manager object and makes changes based on the state read
// and what is in the Manager.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileManager) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Manager")

	// Fetch the Manager instance
	instance, err := GetManager(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("Manager object not found")
			r.status.OnCRNotFound()
			return reconcile.Result{}, nil
		}
		r.status.SetDegraded("Error querying Manager", err.Error())
		return reconcile.Result{}, err
	}
	reqLogger.V(2).Info("Loaded config", "config", instance)
	r.status.OnCRFound()

	if !utils.IsAPIServerReady(r.client, reqLogger) {
		r.status.SetDegraded("Waiting for Tigera API server to be ready", "")
		return reconcile.Result{}, nil
	}

	if !r.licenseAPIReady.IsReady() {
		r.status.SetDegraded("Waiting for LicenseKeyAPI to be ready", "")
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	license, err := utils.FetchLicenseKey(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.status.SetDegraded("License not found", err.Error())
			return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
		}
		r.status.SetDegraded("Error querying license", err.Error())
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Fetch the Installation instance. We need this for a few reasons.
	// - We need to make sure it has successfully completed installation.
	// - We need to get the registry information from its spec.
	variant, installation, err := utils.GetInstallation(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.status.SetDegraded("Installation not found", err.Error())
			return reconcile.Result{}, err
		}
		r.status.SetDegraded("Error querying installation", err.Error())
		return reconcile.Result{}, err
	}

	// Check that if the manager certpair secret exists that it is valid (has key and cert fields)
	// If it does not exist then this function returns a nil secret but no error and a self-signed
	// certificate will be generated when rendering below.
	tlsSecret, err := utils.ValidateCertPair(r.client,
		common.OperatorNamespace(),
		render.ManagerTLSSecretName,
		render.ManagerSecretKeyName,
		render.ManagerSecretCertName,
	)

	// An error is returned in case the read cannot be performed of the secret does not match the expected format
	// In case the secret is not found, the error and the secret will be nil. This check needs to be done for all
	// cluster types. For management cluster, we also need to check if the secret was created before hand.
	if err != nil {
		r.status.SetDegraded("Error validating manager TLS certificate", err.Error())
		return reconcile.Result{}, err
	}

	// If the manager TLS secret exists, check whether it is managed by the
	// operator.
	var operatorManagedCertSecret bool
	if installation.CertificateManagement == nil {
		// We use EnsureCertificateSecret to ensure a secret exists, creating one if one is not passed in.
		// It also ensures the secret passed has the proper DNS names if the secret is operator managed.

		svcDNSNames := dns.GetServiceDNSNames(render.ManagerServiceName, render.ManagerNamespace, r.clusterDomain)
		svcDNSNames = append(svcDNSNames, "localhost")
		certDur := 825 * 24 * time.Hour // 825days*24hours: Create cert with a max expiration that macOS 10.15 will accept
		tlsSecret, operatorManagedCertSecret, err = utils.EnsureCertificateSecret(
			render.ManagerTLSSecretName, tlsSecret, render.ManagerSecretKeyName, render.ManagerSecretCertName, certDur, svcDNSNames...,
		)

		if err != nil {
			r.status.SetDegraded(fmt.Sprintf("Error ensuring manager TLS certificate %q exists and has valid DNS names", render.ManagerTLSSecretName), err.Error())
			return reconcile.Result{}, err
		}

	} else if tlsSecret != nil {
		operatorManagedCertSecret, err = utils.IsCertOperatorIssued(tlsSecret.Data[render.ManagerInternalSecretCertName])
		if err != nil {
			r.status.SetDegraded(fmt.Sprintf("Error checking if manager TLS certificate is operator managed"), err.Error())
			return reconcile.Result{}, err
		}

		if !operatorManagedCertSecret {
			err := fmt.Errorf("user provided secret %s/%s is not supported when certificate management is enabled", render.ManagerNamespace, render.ManagerTLSSecretName)
			r.status.SetDegraded("Invalid certificate configuration", err.Error())
			return reconcile.Result{}, err
		}
	}

	var installCompliance = utils.IsFeatureActive(license, common.ComplianceFeature)
	var complianceServerCertSecret *corev1.Secret

	if installCompliance {
		// Check that compliance is running.
		compliance, err := compliance.GetCompliance(ctx, r.client)
		if err != nil {
			if errors.IsNotFound(err) {
				r.status.SetDegraded("Compliance not found", err.Error())
				return reconcile.Result{}, err
			}
			r.status.SetDegraded("Error querying compliance", err.Error())
			return reconcile.Result{}, err
		}
		if compliance.Status.State != operatorv1.TigeraStatusReady {
			r.status.SetDegraded("Compliance is not ready", fmt.Sprintf("compliance status: %s", compliance.Status.State))
			return reconcile.Result{}, nil
		}

		complianceServerCertSecret, err = utils.ValidateCertPair(r.client,
			common.OperatorNamespace(),
			render.ComplianceServerCertSecret,
			"", // We don't need the key.
			corev1.TLSCertKey,
		)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("failed to retrieve %s", render.ComplianceServerCertSecret))
			r.status.SetDegraded(fmt.Sprintf("Failed to retrieve %s", render.ComplianceServerCertSecret), err.Error())
			return reconcile.Result{}, err
		} else if complianceServerCertSecret == nil {
			reqLogger.Info(fmt.Sprintf("Waiting for secret '%s' to become available", render.ComplianceServerCertSecret))
			r.status.SetDegraded(fmt.Sprintf("Waiting for secret '%s' to become available", render.ComplianceServerCertSecret), "")
			return reconcile.Result{}, nil
		}
	}

	// check that prometheus is running
	ns := &corev1.Namespace{}
	if err = r.client.Get(ctx, client.ObjectKey{Name: common.TigeraPrometheusNamespace}, ns); err != nil {
		if errors.IsNotFound(err) {
			r.status.SetDegraded("tigera-prometheus namespace does not exist", "Dependency on tigera-prometheus not satisfied")
		} else {
			r.status.SetDegraded("Error querying prometheus", err.Error())
		}
		return reconcile.Result{}, err
	}

	pullSecrets, err := utils.GetNetworkingPullSecrets(installation, r.client)
	if err != nil {
		log.Error(err, "Error with Pull secrets")
		r.status.SetDegraded("Error retrieving pull secrets", err.Error())
		return reconcile.Result{}, err
	}

	esClusterConfig, err := utils.GetElasticsearchClusterConfig(context.Background(), r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Elasticsearch cluster configuration is not available, waiting for it to become available")
			r.status.SetDegraded("Elasticsearch cluster configuration is not available, waiting for it to become available", err.Error())
			return reconcile.Result{}, nil
		}
		log.Error(err, "Failed to get the elasticsearch cluster configuration")
		r.status.SetDegraded("Failed to get the elasticsearch cluster configuration", err.Error())
		return reconcile.Result{}, err
	}

	esSecrets, err := utils.ElasticsearchSecrets(ctx, []string{render.ElasticsearchManagerUserSecret}, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Elasticsearch secrets are not available yet, waiting until they become available")
			r.status.SetDegraded("Elasticsearch secrets are not available yet, waiting until they become available", err.Error())
			return reconcile.Result{}, nil
		}
		r.status.SetDegraded("Failed to get Elasticsearch credentials", err.Error())
		return reconcile.Result{}, err
	}

	kibanaPublicCertSecret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: render.KibanaPublicCertSecret, Namespace: common.OperatorNamespace()}, kibanaPublicCertSecret); err != nil {
		reqLogger.Error(err, "Failed to read Kibana public cert secret")
		r.status.SetDegraded("Failed to read Kibana public cert secret", err.Error())
		return reconcile.Result{}, err
	}

	managementCluster, err := utils.GetManagementCluster(ctx, r.client)
	if err != nil {
		log.Error(err, "Error reading ManagementCluster")
		r.status.SetDegraded("Error reading ManagementCluster", err.Error())
		return reconcile.Result{}, err
	}

	managementClusterConnection, err := utils.GetManagementClusterConnection(ctx, r.client)
	if err != nil {
		log.Error(err, "Error reading ManagementClusterConnection")
		r.status.SetDegraded("Error reading ManagementClusterConnection", err.Error())
		return reconcile.Result{}, err
	}

	if managementClusterConnection != nil && managementCluster != nil {
		err = fmt.Errorf("having both a ManagementCluster and a ManagementClusterConnection is not supported")
		log.Error(err, "")
		r.status.SetDegraded(err.Error(), "")
		return reconcile.Result{}, err
	}

	var tunnelSecret *corev1.Secret
	var internalTrafficSecret *corev1.Secret
	if managementCluster != nil {
		// We expect that the secret that holds the certificates for tunnel certificate generation
		// is already created by the Api Server
		tunnelSecret = &corev1.Secret{}
		err := r.client.Get(ctx, client.ObjectKey{Name: render.VoltronTunnelSecretName, Namespace: common.OperatorNamespace()}, tunnelSecret)
		if err != nil {
			r.status.SetDegraded("Failed to check for the existence of management-cluster-connection secret", err.Error())
			return reconcile.Result{}, nil
		}

		// We expect that the secret that holds the certificates for internal communication within the management
		// K8S cluster is already created by the KubeControllers
		internalTrafficSecret = &corev1.Secret{}
		err = r.client.Get(ctx, client.ObjectKey{
			Name:      render.ManagerInternalTLSSecretName,
			Namespace: common.OperatorNamespace(),
		}, internalTrafficSecret)
		if err != nil {
			if errors.IsNotFound(err) {
				r.status.SetDegraded(fmt.Sprintf("Waiting for secret %s in namespace %s to be available", render.ManagerInternalTLSSecretName, common.OperatorNamespace()), "")
				return reconcile.Result{}, nil
			}
			r.status.SetDegraded(fmt.Sprintf("Error fetching TLS secret %s in namespace %s", render.ManagerInternalTLSSecretName, common.OperatorNamespace()), err.Error())
			return reconcile.Result{}, err
		}
	}

	// Fetch the Authentication spec. If present, we use to configure user authentication.
	authenticationCR, err := utils.GetAuthentication(ctx, r.client)
	if err != nil && !errors.IsNotFound(err) {
		r.status.SetDegraded("Error while fetching Authentication", err.Error())
		return reconcile.Result{}, err
	}
	if authenticationCR != nil && authenticationCR.Status.State != operatorv1.TigeraStatusReady {
		r.status.SetDegraded("Authentication is not ready", fmt.Sprintf("authenticationCR status: %s", authenticationCR.Status.State))
		return reconcile.Result{}, nil
	}

	keyValidatorConfig, err := utils.GetKeyValidatorConfig(ctx, r.client, authenticationCR, r.clusterDomain)
	if err != nil {
		log.Error(err, "Failed to process the authentication CR.")
		r.status.SetDegraded("Failed to process the authentication CR.", err.Error())
		return reconcile.Result{}, err
	}

	var elasticLicenseType render.ElasticsearchLicenseType
	if managementClusterConnection == nil {
		if elasticLicenseType, err = utils.GetElasticLicenseType(ctx, r.client, reqLogger); err != nil {
			r.status.SetDegraded("Failed to get Elasticsearch license", err.Error())
			return reconcile.Result{}, err
		}
	}

	var packetCaptureServerCertSecret *corev1.Secret
	packetCaptureServerCertSecret, err = utils.ValidateCertPair(r.client,
		common.OperatorNamespace(),
		render.PacketCaptureCertSecret,
		"", // We don't need the key.
		corev1.TLSCertKey,
	)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("failed to retrieve %s", render.PacketCaptureCertSecret))
		r.status.SetDegraded(fmt.Sprintf("Failed to retrieve %s", render.PacketCaptureCertSecret), err.Error())
		return reconcile.Result{}, err
	} else if packetCaptureServerCertSecret == nil {
		reqLogger.Info(fmt.Sprintf("Waiting for secret '%s' to become available", render.PacketCaptureCertSecret))
		r.status.SetDegraded(fmt.Sprintf("Waiting for secret '%s' to become available", render.PacketCaptureCertSecret), "")
		return reconcile.Result{}, nil
	}

	prometheusCertSecret, err := utils.ValidateCertPair(r.client,
		common.OperatorNamespace(),
		render.PrometheusTLSSecretName,
		"", // We don't need the key.
		corev1.TLSCertKey,
	)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("failed to retrieve %s", render.PrometheusTLSSecretName))
		r.status.SetDegraded(fmt.Sprintf("Failed to retrieve %s", render.PrometheusTLSSecretName), err.Error())
		return reconcile.Result{}, err
	}

	var components []render.Component
	if tlsSecret != nil && operatorManagedCertSecret {
		components = append(components, render.NewPassthrough(tlsSecret))
	}

	// Create a component handler to manage the rendered component.
	handler := utils.NewComponentHandler(log, r.client, r.scheme, instance)

	// Set replicas to 1 for management or managed clusters.
	// TODO Remove after MCM tigera-manager HA deployment is supported.
	var replicas *int32 = installation.ControlPlaneReplicas
	if managementCluster != nil || managementClusterConnection != nil {
		var mcmReplicas int32 = 1
		replicas = &mcmReplicas
	}

	managerCfg := &render.ManagerConfiguration{
		KeyValidatorConfig:            keyValidatorConfig,
		ESSecrets:                     esSecrets,
		KibanaSecrets:                 []*corev1.Secret{kibanaPublicCertSecret},
		ComplianceServerCertSecret:    complianceServerCertSecret,
		PacketCaptureServerCertSecret: packetCaptureServerCertSecret,
		PrometheusCertSecret:          prometheusCertSecret,
		ESClusterConfig:               esClusterConfig,
		TLSKeyPair:                    tlsSecret,
		PullSecrets:                   pullSecrets,
		Openshift:                     r.provider == operatorv1.ProviderOpenShift,
		Installation:                  installation,
		ManagementCluster:             managementCluster,
		TunnelSecret:                  tunnelSecret,
		InternalTrafficSecret:         internalTrafficSecret,
		ClusterDomain:                 r.clusterDomain,
		ESLicenseType:                 elasticLicenseType,
		Replicas:                      replicas,
	}

	// Render the desired objects from the CRD and create or update them.
	component, err := render.Manager(managerCfg)
	if err != nil {
		log.Error(err, "Error rendering Manager")
		r.status.SetDegraded("Error rendering Manager", err.Error())
		return reconcile.Result{}, err
	}

	if err = imageset.ApplyImageSet(ctx, r.client, variant, component); err != nil {
		log.Error(err, "Error with images from ImageSet")
		r.status.SetDegraded("Error with images from ImageSet", err.Error())
		return reconcile.Result{}, err
	}

	components = append(components, component)
	for _, component := range components {
		if err := handler.CreateOrUpdateOrDelete(ctx, component, r.status); err != nil {
			r.status.SetDegraded("Error creating / updating resource", err.Error())
			return reconcile.Result{}, err
		}
	}

	// Clear the degraded bit if we've reached this far.
	r.status.ClearDegraded()
	instance.Status.State = operatorv1.TigeraStatusReady
	if r.status.IsAvailable() {
		if err = r.client.Status().Update(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}
