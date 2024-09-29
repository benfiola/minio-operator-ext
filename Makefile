ASSETS ?= $(shell pwd)/.dev
DEV ?= $(shell pwd)/dev
HELM_VERSION ?= 3.16.1
KIND_CLUSTER_NAME ?= minio-operator-ext
KIND_VERSION ?= 0.24.0
KUBERNETES_VERSION ?= 1.30.5
MC_VERSION ?= 2024-08-17T11-33-50Z

OS = $(shell go env GOOS)
ARCH = $(shell go env GOARCH)

HELM = $(ASSETS)/helm
HELM_CMD = $(HELM)
HELM_URL = https://get.helm.sh/helm-v$(HELM_VERSION)-$(OS)-$(ARCH).tar.gz
KIND = $(ASSETS)/kind
KIND_CMD = KUBECONFIG=$(KUBECONFIG) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) kind
KIND_URL = https://github.com/kubernetes-sigs/kind/releases/download/v$(KIND_VERSION)/kind-$(OS)-$(ARCH)
KUBECONFIG = $(ASSETS)/kube-config.yaml
KUBECTL = $(ASSETS)/kubectl
KUBECTL_CMD = KUBECONFIG=$(KUBECONFIG) $(ASSETS)/kubectl
KUBECTL_URL = https://dl.k8s.io/release/v$(KUBERNETES_VERSION)/bin/$(OS)/$(ARCH)/kubectl
MC = $(ASSETS)/mc 
MC_CMD = MINIO_INSECURE=1 MINIO_DISABLE_PAGER=1 $(MC)
MC_URL = https://dl.min.io/client/mc/release/$(OS)-$(ARCH)/archive/mc.RELEASE.$(MC_VERSION)
MINIO_OPERATOR_MANIFEST = $(DEV)/minio-operator.yaml
MINIO_TENANT_MANIFEST = $(DEV)/minio-tenant.yaml
OPENLDAP_MANIFEST = $(DEV)/openldap.yaml

# MAYBE_CREATE_KIND_CLUSTER is a conditional target that is set to 'create-kind-cluster' only if a matching cluster doesn't exist
MAYBE_CREATE_KIND_CLUSTER = create-kind-cluster
ifneq (,$(wildcard $(KIND)))
	ifneq (,$(shell $(KIND_CMD) get clusters | grep $(KIND_CLUSTER_NAME)))
		MAYBE_CREATE_KIND_CLUSTER = 
	endif
endif

.PHONY: default
default: 

# NOTE: ensure 'delete-tools' is final target.  other targets might have tool dependencies (e.g., 'delete-kind-cluster')
.PHONY: clean
clean: delete-kind-cluster delete-minio-manifests delete-tools

.PHONY: create-cluster
create-cluster: $(MAYBE_CREATE_KIND_CLUSTER) apply-manifests

.PHONY: apply-manifests
apply-manifests: $(KUBECTL) $(MINIO_OPERATOR_MANIFEST) $(MINIO_TENANT_MANIFEST)
	# apply minio operator manifest
	$(KUBECTL_CMD) apply -f $(MINIO_OPERATOR_MANIFEST)
	# apply minio tenant manifest
	$(KUBECTL_CMD) apply -f $(MINIO_TENANT_MANIFEST)
	# apply openldan manifest
	$(KUBECTL_CMD) apply -f $(OPENLDAP_MANIFEST)

.PHONY: unapply-manifests
unapply-manifests: $(KUBECTL) $(MINIO_OPERATOR_MANIFEST) $(MINIO_TENANT_MANIFEST)
	# delete resources created by openldap manifest
	$(KUBECTL_CMD) delete -f $(OPENLDAP_MANIFEST) --ignore-not-found=true
	# delete resources created by minio tenant manifest
	$(KUBECTL_CMD) delete -f $(MINIO_TENANT_MANIFEST) --ignore-not-found=true
	# delete resources created by minio operator manifest
	$(KUBECTL_CMD) delete -f $(MINIO_OPERATOR_MANIFEST) --ignore-not-found=true

.PHONY: create-minio-manifests
create-minio-manifests: $(MINIO_OPERATOR_MANIFEST) $(MINIO_TENANT_MANIFEST)

.PHONY: delete-minio-manifests
delete-minio-manifests:
	rm -f $(MINIO_OPERATOR_MANIFEST)
	rm -f $(MINIO_TENANT_MANIFEST)

.PHONY: create-kind-cluster
create-kind-cluster: $(KIND)
	# create kind cluster
	# NOTE: uses a temporary kubeconfig path - kind tries to acquire a file lock on the kubeconfig file.  if $(KUBECONFIG) is on a virtiofs mount, this will fail.
	$(KIND_CMD) create cluster --kubeconfig /tmp/kube-config.yaml
	# copy temp file to actual kubeconfig location
	mv /tmp/kube-config.yaml $(KUBECONFIG)

.PHONY: delete-kind-cluster
delete-kind-cluster: $(KIND)
	# delete kind cluster
	# NOTE: uses a temporary kubeconfig path - kind tries to acquire a file lock on the kubeconfig file.  if $(KUBECONFIG) is on a virtiofs mount, this will fail.
	$(KIND_CMD) delete cluster --kubeconfig /tmp/kube-config.yaml
	# remove kubeconfig
	rm -f $(KUBECONFIG)

.PHONY: install-tools
install-tools: $(HELM) $(KIND) $(KUBECTL) $(MC)

.PHONY: delete-tools
delete-tools:
	# removing .dev directory
	rm -rf $(ASSETS)

.PHONY: set-minio-identity-provider-builtin
set-minio-identity-provider-builtin: $(MC)
	# add ldap identity provider
	$(MC_CMD) idp ldap add local server_addr=openldap.openldap.svc:389 lookup_bind_dn=cn='ldap-admin,dc=example,dc=org' lookup_bind_password=ldap-admin user_dn_search_base_dn='ou=users,dc=example,dc=org' user_dn_search_filter='(&(objectClass=posixAccount)(uid=%s))' group_search_base_dn='ou=users,dc=example,dc=org' group_search_filter='(&(objectClass=groupOfNames)(member=%d))' server_insecure=on
	# restart minio tenant
	$(MC_CMD) admin service restart local --json

.PHONY: set-minio-identity-provider-ldap
set-minio-identity-provider-ldap: $(MC)
	# remove ldap identity provider
	$(MC_CMD) idp ldap remove local
	# restart minio tenant
	$(MC_CMD) admin service restart local --json

$(ASSETS):
	# create .dev directory
	mkdir -p $(ASSETS)

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

$(MINIO_OPERATOR_MANIFEST): $(KUBECTL) $(HELM)
	# generate minio operator manifest
	# add namespace to manifest
	$(KUBECTL_CMD) create namespace minio-operator --dry-run=client --output=yaml > $(MINIO_OPERATOR_MANIFEST)
	# add helm template to manifest
	$(HELM_CMD) template --namespace minio-operator --repo=https://operator.min.io operator operator >> $(MINIO_OPERATOR_MANIFEST)

$(MINIO_TENANT_MANIFEST): $(HELM)
	# generate minio tenant manifest
	# add helm template to manifest
	$(HELM_CMD) template --repo=https://operator.min.io --set="tenant.pools[0].name=default" --set="tenant.pools[0].servers=1" --set="tenant.pools[0].volumesPerServer=1" --set="tenant.pools[0].size=100Mi" tenant tenant > $(MINIO_TENANT_MANIFEST)
