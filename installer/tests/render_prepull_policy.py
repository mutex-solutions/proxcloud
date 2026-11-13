#!/usr/bin/env python3
"""Render the sandbox prepull manifests (stage 55, templates/sandbox-prepull.yml.j2) with fixture values into the file
named by argv[1], for the CEL admission-policy test in test/policy (k8s.io/apiserver, no cluster). Fixture only: no
site files, no secrets."""
import copy
import os
import sys
from pathlib import Path

import yaml

os.chdir(Path(__file__).resolve().parent.parent)
sys.path.insert(0, "filter_plugins")
import proxcloud as f
from ansible.parsing.dataloader import DataLoader
from ansible.plugins.loader import filter_loader, init_plugin_loader
from ansible.template import Templar
init_plugin_loader(); filter_loader.add_directory("filter_plugins")
D = yaml.safe_load(Path("playbooks/group_vars/all/defaults.yml").read_text())["pc_defaults"]
P = yaml.safe_load(Path("playbooks/group_vars/all/prepull-policy.yml").read_text())
pc = copy.deepcopy(D); pc["sandbox"]["registry"]["address"] = "192.0.2.226"
imgs = f.sandbox_images({"base": "ghcr.io/mutex-solutions/sandbox-base:20261101-aaaaaaa@sha256:" + "1"*64}, "192.0.2.226")
t = Templar(loader=DataLoader(), variables=dict(pc=pc, pc_sbx_images=imgs, pc_sbx_reg_ca="CA", pc_ver={"pause_image": "pause"}, **P))
Path(sys.argv[1]).write_text(t.template(Path("playbooks/templates/sandbox-prepull.yml.j2").read_text()))
