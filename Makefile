# NOTE: KIND_NODE_IMAGE, KIND_VERSION, KUBERNETES_VERSION are coupled
# NOTE: MINIO_VERSION, MC_VERSION are coupled
ASSETS ?= $(shell pwd)/.dev
DEV ?= $(shell pwd)/dev
CLOUD_PROVIDER_KIND_VERSION ?= 0.4.0
HELM_VERSION ?= 3.16.1
KIND_CLUSTER_NAME ?= minio-operator-ext
KIND_NODE_IMAGE ?= kindest/node:v1.30.4@sha256:976ea815844d5fa93be213437e3ff5754cd599b040946b5cca43ca45c2047114
KIND_VERSION ?= 0.24.0
KUBERNETES_VERSION ?= 1.30.4
LB_HOSTS_MANAGER_VERSION ?= 0.1.0
MINIO_VERSION ?= 6.0.3
MC_VERSION ?= 2024-08-17T11-33-50Z

OS = $(shell go env GOOS)
ARCH = $(shell go env GOARCH)
MANIFESTS = $(shell pwd)/manifests

CLOUD_PROVIDER_KIND = $(ASSETS)/cloud-provider-kind
CLOUD_PROVIDER_KIND_CMD = $(CLOUD_PROVIDER_KIND)
CLOUD_PROVIDER_KIND_LOG = $(ASSETS)/cloud-provider-kind.log
CLOUD_PROVIDER_KIND_URL = https://github.com/kubernetes-sigs/cloud-provider-kind/releases/download/v$(CLOUD_PROVIDER_KIND_VERSION)/cloud-provider-kind_$(CLOUD_PROVIDER_KIND_VERSION)_$(OS)_$(ARCH).tar.gz
CRDS_MANIFEST = $(ASSETS)/crds.yaml
CRDS_MANIFEST_SRC = $(MANIFESTS)/crds.yaml
HELM = $(ASSETS)/helm
HELM_CMD = env $(HELM)
HELM_URL = https://get.helm.sh/helm-v$(HELM_VERSION)-$(OS)-$(ARCH).tar.gz
KIND = $(ASSETS)/kind
KIND_CMD = env KUBECONFIG=$(KUBECONFIG) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) kind
KIND_URL = https://github.com/kubernetes-sigs/kind/releases/download/v$(KIND_VERSION)/kind-$(OS)-$(ARCH)
KUBECONFIG = $(ASSETS)/kube-config.yaml
KUBECTL = $(ASSETS)/kubectl
KUBECTL_CMD = env KUBECONFIG=$(KUBECONFIG) $(ASSETS)/kubectl
KUBECTL_URL = https://dl.k8s.io/release/v$(KUBERNETES_VERSION)/bin/$(OS)/$(ARCH)/kubectl
LB_HOSTS_MANAGER = $(ASSETS)/lb-hosts-manager
LB_HOSTS_MANAGER_CMD = env KUBECONFIG=$(KUBECONFIG) $(LB_HOSTS_MANAGER)
LB_HOSTS_MANAGER_LOG = $(ASSETS)/lb-hosts-manager.log
LB_HOSTS_MANAGER_URL = https://github.com/benfiola/lb-hosts-manager/releases/download/v$(LB_HOSTS_MANAGER_VERSION)/lb-hosts-manager-$(OS)-$(ARCH)
MC = $(ASSETS)/mc 
MC_CMD = env MINIO_INSECURE=1 MINIO_DISABLE_PAGER=1 $(MC)
MC_URL = https://dl.min.io/client/mc/release/$(OS)-$(ARCH)/archive/mc.RELEASE.$(MC_VERSION)
MINIO_OPERATOR_MANIFEST = $(ASSETS)/minio-operator.yaml
MINIO_TENANT_MANIFEST = $(ASSETS)/minio-tenant.yaml
OPENLDAP_MANIFEST = $(ASSETS)/openldap.yaml
OPENLDAP_MANIFEST_SRC = $(DEV)/openldap.yaml

# MAYBE_CREATE_KIND_CLUSTER is a conditional target that is set to 'create-kind-cluster' only if a matching cluster doesn't exist
MAYBE_CREATE_KIND_CLUSTER = create-kind-cluster
ifneq (,$(wildcard $(KIND)))
	ifneq (,$(shell $(KIND_CMD) get clusters | grep $(KIND_CLUSTER_NAME)))
		MAYBE_CREATE_KIND_CLUSTER = 
	endif
endif

.PHONY: default
default: 

.PHONY: clean
clean: delete-kind-cluster
	# delete asset directory
	rm -rf $(ASSETS)

.PHONY: create-cluster
create-cluster: $(MAYBE_CREATE_KIND_CLUSTER) get-kind-cluster-kubeconfig apply-manifests start-cloud-provider-kind start-lb-hosts-manager wait-for-ready

.PHONY: get-kind-cluster-kubeconfig
get-kind-cluster-kubeconfig: $(KIND) | $(ASSETS)
	# delete existing kubeconfigs
	rm -rf $(KUBECONFIG) /tmp/kube-config.yaml
	# export kind cluster kubeconfig to temporary location
	# NOTE: uses a temporary kubeconfig path - kind tries to acquire a file lock on the kubeconfig file.  if $(KUBECONFIG) is on a virtiofs mount, this will fail.
	$(KIND_CMD) export kubeconfig --kubeconfig /tmp/kube-config.yaml
	# move kubeconfig to correct location
	mv /tmp/kube-config.yaml $(KUBECONFIG)

# NOTE: assumes that cluster is already created
.PHONY: apply-manifests
apply-manifests: $(KUBECTL) $(CRDS_MANIFEST) $(MINIO_OPERATOR_MANIFEST) $(MINIO_TENANT_MANIFEST) $(OPENLDAP_MANIFEST)
	# apply minio operator manifest
	$(KUBECTL_CMD) apply -f $(MINIO_OPERATOR_MANIFEST)
	# apply minio tenant manifest
	$(KUBECTL_CMD) apply -f $(MINIO_TENANT_MANIFEST)
	# apply openldap manifest
	$(KUBECTL_CMD) apply -f $(OPENLDAP_MANIFEST)
	# apply crds
	$(KUBECTL_CMD) apply -f $(CRDS_MANIFEST)

# NOTE: assumes that cluster is already created
.PHONY: unapply-manifests
unapply-manifests: $(KUBECTL) $(MINIO_OPERATOR_MANIFEST) $(MINIO_TENANT_MANIFEST)
	# delete resources created by openldap manifest
	$(KUBECTL_CMD) delete -f $(OPENLDAP_MANIFEST) --ignore-not-found=true
	# delete resources created by minio tenant manifest
	$(KUBECTL_CMD) delete -f $(MINIO_TENANT_MANIFEST) --ignore-not-found=true
	# delete resources created by minio operator manifest
	$(KUBECTL_CMD) delete -f $(MINIO_OPERATOR_MANIFEST) --ignore-not-found=true

.PHONY: create-kind-cluster
create-kind-cluster: $(KIND)
	# create kind cluster
	# NOTE: uses a temporary kubeconfig path - kind tries to acquire a file lock on the kubeconfig file.  if $(KUBECONFIG) is on a virtiofs mount, this will fail.
	$(KIND_CMD) create cluster --kubeconfig /tmp/kube-config.yaml --image $(KIND_NODE_IMAGE)
	# remove temporary kubeconfig
	rm -f /tmp/kube-config.yaml

.PHONY: delete-kind-cluster
delete-kind-cluster: $(KIND)
	# delete kind cluster
	# NOTE: uses a temporary kubeconfig path - kind tries to acquire a file lock on the kubeconfig file.  if $(KUBECONFIG) is on a virtiofs mount, this will fail.
	$(KIND_CMD) delete cluster --kubeconfig /tmp/kube-config.yaml
	# delete kubeconfig
	rm -f $(KUBECONFIG)

.PHONY: install-tools
install-tools: $(CLOUD_PROVIDER_KIND) $(HELM) $(KIND) $(LB_HOSTS_MANAGER) $(KUBECTL) $(MC)

# NOTE: assumes that minio is deployed and accesible from host
.PHONY: set-minio-identity-provider-builtin
set-minio-identity-provider-builtin: $(MC)
	# add ldap identity provider
	$(MC_CMD) idp ldap add local server_addr=openldap.openldap.svc:389 lookup_bind_dn=cn='ldap-admin,dc=example,dc=org' lookup_bind_password=ldap-admin user_dn_search_base_dn='ou=users,dc=example,dc=org' user_dn_search_filter='(&(objectClass=posixAccount)(uid=%s))' group_search_base_dn='ou=users,dc=example,dc=org' group_search_filter='(&(objectClass=groupOfNames)(member=%d))' server_insecure=on
	# restart minio tenant
	$(MC_CMD) admin service restart local --json

# NOTE: assumes that minio is deployed and accesible from host
.PHONY: set-minio-identity-provider-ldap
set-minio-identity-provider-ldap: $(MC)
	# remove ldap identity provider
	$(MC_CMD) idp ldap remove local
	# restart minio tenant
	$(MC_CMD) admin service restart local --json

.PHONY: start-cloud-provider-kind
start-cloud-provider-kind: $(CLOUD_PROVIDER_KIND)
	# send SIGTERM to existing cloud-provider-kind
	pkill -x -f $(CLOUD_PROVIDER_KIND) || true
	# wait for cloud-provider-kind to exit
	while true; do pgrep -x -f $(CLOUD_PROVIDER_KIND) || break; sleep 1; done
	# launch cloud-provider-kind
	nohup $(CLOUD_PROVIDER_KIND_CMD) > $(CLOUD_PROVIDER_KIND_LOG) 2>&1 &

.PHONY: start-lb-hosts-manager
start-lb-hosts-manager: $(LB_HOSTS_MANAGER)
	# send SIGTERM to existing lb-hosts-manager
	pkill -x -f $(LB_HOSTS_MANAGER) || true
	# wait for lb-hosts-manager to exit
	while true; do pgrep -x -f $(LB_HOSTS_MANAGER) || break; sleep 1; done
	# launch lb-hosts-manager
	nohup $(LB_HOSTS_MANAGER_CMD) run > $(LB_HOSTS_MANAGER_LOG) 2>&1 &

.PHONY: wait-for-ready
wait-for-ready:
	# wait for minio to be connectable
	while true; do curl -I --insecure https://minio.default.svc > /dev/null 2>&1 && break; sleep 1; done;

$(ASSETS):
	# create .dev directory
	mkdir -p $(ASSETS)

$(CLOUD_PROVIDER_KIND): | $(ASSETS)
	# install cloud-provider-kind
	# create extract directory
	mkdir -p $(ASSETS)/.tmp
	# download archive
	curl -o $(ASSETS)/.tmp/archive.tar.gz -fsSL $(CLOUD_PROVIDER_KIND_URL)
	# extract archive
	tar xzf $(ASSETS)/.tmp/archive.tar.gz -C $(ASSETS)/.tmp
	# copy executable
	cp $(ASSETS)/.tmp/cloud-provider-kind $(ASSETS)/cloud-provider-kind
	# delete extract directory
	rm -rf $(ASSETS)/.tmp

$(CRDS_MANIFEST): | $(ASSETS)
	# copy crd manifests
	cp $(CRDS_MANIFEST_SRC) $(CRDS_MANIFEST)

$(HELM): | $(ASSETS)
	# install helm
	# create extract directory
	mkdir -p $(ASSETS)/.tmp
	# download archive
	curl -o $(ASSETS)/.tmp/archive.tar.gz -fsSL $(HELM_URL)
	# extract archive
	tar xzf $(ASSETS)/.tmp/archive.tar.gz -C $(ASSETS)/.tmp --strip-components 1
	# copy executable
	cp $(ASSETS)/.tmp/helm $(ASSETS)/helm
	# delete extract directory
	rm -rf $(ASSETS)/.tmp

$(LB_HOSTS_MANAGER): | $(ASSETS)
	# install lb-hosts-manager
	# download
	curl -o $(LB_HOSTS_MANAGER) -fsSL $(LB_HOSTS_MANAGER_URL)
	# make executable
	chmod +x $(LB_HOSTS_MANAGER)

$(KIND): | $(ASSETS)
	# install kind
	# download
	curl -o $(KIND) -fsSL $(KIND_URL)
	# make executable
	chmod +x $(KIND)

$(KUBECTL): | $(ASSETS)
	# install kubectl
	# download
	curl -o $(KUBECTL) -fsSL $(KUBECTL_URL)
	# make kubectl executable
	chmod +x $(KUBECTL)

$(MC): | $(ASSETS)
	# install mc
	# download
	curl -o $(MC) -fsSL $(MC_URL)
	# make executable
	chmod +x $(MC)

$(MINIO_OPERATOR_MANIFEST): $(KUBECTL) $(HELM) | $(ASSETS)
	# generate minio operator manifest
	# add namespace to manifest
	$(KUBECTL_CMD) create namespace minio-operator --dry-run=client --output=yaml > $(MINIO_OPERATOR_MANIFEST)
	# add helm template to manifest
	$(HELM_CMD) template --namespace minio-operator --repo=https://operator.min.io --version=$(MINIO_VERSION) operator operator >> $(MINIO_OPERATOR_MANIFEST)

$(MINIO_TENANT_MANIFEST): $(HELM) | $(ASSETS)
	# generate minio tenant manifest
	# add helm template to manifest
	$(HELM_CMD) template --repo=https://operator.min.io --version=$(MINIO_VERSION) --set="tenant.exposeServices.minio=true" --set="tenant.pools[0].name=default" --set="tenant.pools[0].servers=1" --set="tenant.pools[0].volumesPerServer=1" --set="tenant.pools[0].size=100Mi" tenant tenant > $(MINIO_TENANT_MANIFEST)

$(OPENLDAP_MANIFEST): | $(ASSETS)
	# copy openldap manifest
	cp $(OPENLDAP_MANIFEST_SRC) $(OPENLDAP_MANIFEST)
