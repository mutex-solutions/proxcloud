# ProxCloud installer. One entrypoint, every stage idempotent: see docs/install.md.
#   make install INVENTORY=inventory/mysite
INVENTORY ?= inventory/example
VENV      := $(CURDIR)/.venv
PLAYBOOK  := ANSIBLE_CONFIG=$(CURDIR)/ansible.cfg $(VENV)/bin/ansible-playbook -i $(abspath $(INVENTORY))/hosts.yml -e pc_site_dir=$(abspath $(INVENTORY))
ARGS      ?=

.PHONY: deps preflight install storage-rebench templates upgrade identity-check add-node remove-node mgmt-scale rustfs-upgrade rustfs-rollback test
$(VENV)/bin/ansible-playbook: requirements.txt
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install -q -r requirements.txt
	touch $@
deps: $(VENV)/bin/ansible-playbook

preflight: deps       ; $(PLAYBOOK) playbooks/00-preflight.yml $(ARGS)
install: deps         ; $(PLAYBOOK) playbooks/site.yml $(ARGS)
# Re-run the fio class suggestion (also part of add-node); never changes a live class.
storage-rebench: deps ; $(PLAYBOOK) playbooks/00-preflight.yml -e pc_rebench=true $(ARGS)
# Build new OS template versions now, even when one exists.
templates: deps       ; $(PLAYBOOK) playbooks/00-preflight.yml playbooks/20-templates.yml -e pc_templates_force=true $(ARGS)
# Bump installer/playbooks/group_vars/all/versions.yml first (or -e), then:
# (stage 10 too: a release's grants change with it, e.g. the services token's ISO stores.)
upgrade: deps         ; $(PLAYBOOK) playbooks/00-preflight.yml playbooks/10-pve-identity.yml playbooks/30-mgmt-cluster.yml playbooks/35-local-images.yml playbooks/40-mgmt-platform.yml playbooks/60-register.yml $(ARGS)
# What stage 10 would grant, align and take away, writing nothing: the items skipped with "does not support check
# mode" are the changes, and the last task prints which ISO stores the services token sees today.
identity-check: deps  ; $(PLAYBOOK) playbooks/10-pve-identity.yml --check -v $(ARGS)
# make add-node NODE=pve4 [NODE_IP=10.0.0.14]: the host must be in the inventory group `pve`.
add-node: deps        ; $(PLAYBOOK) playbooks/70-add-node.yml -e pc_node=$(NODE) $(ARGS)
remove-node: deps     ; $(PLAYBOOK) playbooks/71-remove-node.yml -e pc_node=$(NODE) $(ARGS)
# Add the new servers to the inventory group `mgmt` (ssh) or raise mgmt.servers (pve-vm), then:
mgmt-scale: deps      ; $(PLAYBOOK) playbooks/00-preflight.yml playbooks/25-mgmt-vms.yml playbooks/30-mgmt-cluster.yml $(ARGS)
rustfs-upgrade: deps  ; $(PLAYBOOK) playbooks/46-rustfs-upgrade.yml $(if $(VERSION),-e pc_rustfs_version=$(VERSION)) $(ARGS)
rustfs-rollback: deps ; $(PLAYBOOK) playbooks/46-rustfs-upgrade.yml -e pc_rustfs_rollback=true $(ARGS)
test: deps            ; $(VENV)/bin/python tests/test_filters.py && $(VENV)/bin/python tests/test_icmp_guard.py
