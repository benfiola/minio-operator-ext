#!/bin/sh -e
# configures the development minio tenant with a local ldap server
echo "adding ldap server"
mc idp ldap add local \
    server_addr=openldap.openldap.svc:389 \
    lookup_bind_dn=cn='ldap-admin,dc=example,dc=org' \
    lookup_bind_password=ldap-admin \
    user_dn_search_base_dn='ou=users,dc=example,dc=org' \
    user_dn_search_filter='(&(objectClass=posixAccount)(uid=%s))' \
    group_search_base_dn='ou=users,dc=example,dc=org' \
    group_search_filter='(&(objectClass=groupOfNames)(member=%d))' \
    server_insecure=on

echo "restarting tenant"
mc admin service restart local
