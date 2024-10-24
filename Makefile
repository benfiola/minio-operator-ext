# NOTE: MINIO_VERSION, MC_VERSION are coupled
ASSETS ?= $(shell pwd)/.dev
DEV ?= $(shell pwd)/dev
CONTROLLER_GEN_VERSION ?= 0.16.4
HELM_VERSION ?= 3.16.1
KUBERNETES_VERSION ?= 1.30.4
LB_HOSTS_MANAGER_VERSION ?= 0.1.0
MINIO_VERSION ?= 6.0.3
MC_VERSION ?= 2024-08-17T11-33-50Z
MINIKUBE_VERSION ?= 1.34.0

OS = $(shell go env GOOS)
ARCH = $(shell go env GOARCH)
MANIFESTS = $(shell pwd)/manifests

CONTROLLER_GEN = $(ASSETS)/controller-gen
CONTROLLER_GEN_URL = https://github.com/kubernetes-sigs/controller-tools/releases/download/v$(CONTROLLER_GEN_VERSION)/controller-gen-$(OS)-$(ARCH)
CRDS_MANIFEST = $(ASSETS)/crds.yaml
CRDS_MANIFEST_SRC = $(MANIFESTS)/crds.yaml
HELM = $(ASSETS)/helm
HELM_CMD = env $(HELM)
HELM_URL = https://get.helm.sh/helm-v$(HELM_VERSION)-$(OS)-$(ARCH).tar.gz
KUBECONFIG = $(ASSETS)/kube-config.yaml
KUBECTL = $(ASSETS)/kubectl
KUBECTL_CMD = env KUBECONFIG=$(KUBECONFIG) $(ASSETS)/kubectl
KUBECTL_URL = https://dl.k8s.io/release/v$(KUBERNETES_VERSION)/bin/$(OS)/$(ARCH)/kubectl
KUSTOMIZE_CMD = $(KUBECTL_CMD) kustomize --enable-helm --load-restrictor=LoadRestrictionsNone
LB_HOSTS_MANAGER = $(ASSETS)/lb-hosts-manager
LB_HOSTS_MANAGER_CMD = env KUBECONFIG=$(KUBECONFIG) $(LB_HOSTS_MANAGER)
LB_HOSTS_MANAGER_LOG = $(ASSETS)/lb-hosts-manager.log
LB_HOSTS_MANAGER_URL = https://github.com/benfiola/lb-hosts-manager/releases/download/v$(LB_HOSTS_MANAGER_VERSION)/lb-hosts-manager-$(OS)-$(ARCH)
MC = $(ASSETS)/mc 
MC_CMD = env MINIO_INSECURE=1 MINIO_DISABLE_PAGER=1 $(MC)
MC_URL = https://dl.min.io/client/mc/release/$(OS)-$(ARCH)/archive/mc.RELEASE.$(MC_VERSION)
MINIKUBE = $(ASSETS)/minikube
MINIKUBE_CMD = env KUBECONFIG=$(KUBECONFIG) MINIKUBE_HOME=$(ASSETS)/.minikube $(MINIKUBE)
MINIKUBE_TUNNEL_LOG = $(ASSETS)/.minikube/tunnel.log
MINIKUBE_URL = https://github.com/kubernetes/minikube/releases/download/v$(MINIKUBE_VERSION)/minikube-$(OS)-$(ARCH)
MINIO_OPERATOR_MANIFEST = $(ASSETS)/minio-operator.yaml
MINIO_OPERATOR_MANIFEST_SRC = $(DEV)/manifests/minio-operator
MINIO_TENANT_MANIFEST = $(ASSETS)/minio-tenant.yaml
MINIO_TENANT_MANIFEST_SRC = $(DEV)/manifests/minio-tenant
OPENLDAP_MANIFEST = $(ASSETS)/openldap.yaml
OPENLDAP_MANIFEST_SRC = $(DEV)/manifests/openldap

.PHONY: default
default: 

.PHONY: clean
clean: delete-minikube-cluster
	# delete asset directory
	rm -rf $(ASSETS)

.PHONY: dev-env
dev-env: create-minikube-cluster apply-manifests start-minikube-tunnel start-lb-hosts-manager wait-for-ready

.PHONY: e2e-test
e2e-test:
	go test -count=1 -v ./internal/e2e

.PHONY: create-minikube-cluster
create-minikube-cluster: $(MINIKUBE)
	# create minikube cluster
	$(MINIKUBE_CMD) start --force --kubernetes-version=$(KUBERNETES_VERSION)

.PHONY: delete-minikube-cluster
delete-minikube-cluster: $(MINIKUBE)
	# delete minikube cluster
	$(MINIKUBE_CMD) delete || true

.PHONY: set-minio-identity-provider-builtin
set-minio-identity-provider-builtin: $(MC)
	# add ldap identity provider
	$(MC_CMD) idp ldap add local server_addr=openldap.default.svc:389 lookup_bind_dn=cn='ldap-admin,dc=example,dc=org' lookup_bind_password=ldap-admin user_dn_search_base_dn='ou=users,dc=example,dc=org' user_dn_search_filter='(&(objectClass=posixAccount)(uid=%s))' group_search_base_dn='ou=users,dc=example,dc=org' group_search_filter='(&(objectClass=groupOfNames)(member=%d))' server_insecure=on
	# restart minio tenant
	$(MC_CMD) admin service restart local --json

.PHONY: set-minio-identity-provider-ldap
set-minio-identity-provider-ldap: $(MC)
	# remove ldap identity provider
	$(MC_CMD) idp ldap remove local
	# restart minio tenant
	$(MC_CMD) admin service restart local --json

.PHONY: start-lb-hosts-manager
start-lb-hosts-manager: $(LB_HOSTS_MANAGER)
	# send SIGTERM to existing lb-hosts-managers
	pkill -x -f '$(LB_HOSTS_MANAGER) run' || true
	# wait for existing lb-hosts-managers to exit
	while true; do pgrep -x -f '$(LB_HOSTS_MANAGER) run' || break; sleep 1; done
	# launch lb-hosts-manager
	nohup $(LB_HOSTS_MANAGER_CMD) run > $(LB_HOSTS_MANAGER_LOG) 2>&1 &

.PHONY: start-minikube-tunnel
start-minikube-tunnel: $(LB_HOSTS_MANAGER)
	# send SIGTERM to existing minikube tunnels
	pkill -x -f '$(MINIKUBE) tunnel' || true
	# wait for existing minikube tunnels to exit
	while true; do pgrep -x -f '$(MINIKUBE) tunnel' || break; sleep 1; done
	# launch minikube tunnel
	nohup $(MINIKUBE_CMD) tunnel > $(MINIKUBE_TUNNEL_LOG) 2>&1 &

.PHONY: wait-for-ready
wait-for-ready:
	# wait for minio to be connectable
	while true; do curl --max-time 2 -I --insecure https://minio.default.svc > /dev/null 2>&1 && break; sleep 1; done;

.PHONY: generate-code
generate-code: $(CONTROLLER_GEN)
	# generate deepcopy
	$(CONTROLLER_GEN) object paths=./pkg/api/bfiola.dev/v1

$(ASSETS):
	# create .dev directory
	mkdir -p $(ASSETS)

.PHONY: install-tools
install-tools: $(CONTROLLER_GEN) $(HELM) $(LB_HOSTS_MANAGER) $(KUBECTL) $(MC) $(MINIKUBE)

$(CONTROLLER_GEN): | $(ASSETS)
	# install controller-gen
	# download
	curl -o $(CONTROLLER_GEN) -fsSL $(CONTROLLER_GEN_URL)
	# make executable
	chmod +x $(CONTROLLER_GEN)

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

$(MINIKUBE): | $(ASSETS)
	# install minikube
	# download
	curl -o $(MINIKUBE) -fsSL $(MINIKUBE_URL)
	# make executable
	chmod +x $(MINIKUBE)

# MANIFESTS

.PHONY: apply-manifests
apply-manifests: $(CRDS_MANIFEST) $(KUBECTL) $(MINIO_OPERATOR_MANIFEST) $(MINIO_TENANT_MANIFEST) $(OPENLDAP_MANIFEST)
	# deploy minio operator
	$(KUBECTL_CMD) apply -f $(MINIO_OPERATOR_MANIFEST)
	# deploy minio tenant
	$(KUBECTL_CMD) apply -f $(MINIO_TENANT_MANIFEST)
	# deploy openldap
	$(KUBECTL_CMD) apply -f $(OPENLDAP_MANIFEST)
	# deploy crds
	$(KUBECTL_CMD) apply -f $(CRDS_MANIFEST)

.PHONY: generate-manifests
generate-manifests: $(CRDS_MANIFEST) $(MINIO_OPERATOR_MANIFEST) $(MINIO_TENANT_MANIFEST) $(OPENLDAP_MANIFEST)

$(CRDS_MANIFEST): | $(ASSETS)
	# copy crds manifest
	cp $(CRDS_MANIFEST_SRC) $(CRDS_MANIFEST)

$(MINIO_OPERATOR_MANIFEST): $(KUBECTL) $(HELM) | $(ASSETS)
	# generate minio operator manifest
	$(KUSTOMIZE_CMD) $(MINIO_OPERATOR_MANIFEST_SRC) > $(MINIO_OPERATOR_MANIFEST)

$(MINIO_TENANT_MANIFEST): $(KUBECTL) $(HELM) | $(ASSETS)
	# generate minio operator manifest
	$(KUSTOMIZE_CMD) $(MINIO_TENANT_MANIFEST_SRC) > $(MINIO_TENANT_MANIFEST)

$(OPENLDAP_MANIFEST): $(KUBECTL) | $(ASSETS)
	# generate openldap manifest
	$(KUSTOMIZE_CMD) $(OPENLDAP_MANIFEST_SRC) > $(OPENLDAP_MANIFEST)
