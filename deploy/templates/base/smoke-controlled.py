#!/usr/local/bin/python3
"""Fixed offline build gate for the NONDEFAULT controlled candidate; not parity/CVE acceptance."""
import ctypes
import importlib.util
from importlib.metadata import version
import json
import os
from pathlib import Path
import pwd
import shlex
import sqlite3
import ssl
import subprocess
import sys
import sysconfig
import tempfile

MARKER = "PROXCLOUD_CONTROLLED_BASE_OK"
VERSIONS = {"node": "v22.23.5", "npm": "11.21.2", "npx": "11.21.2",
            "corepack": "0.36.0", "yarn": "1.22.22", "yarnpkg": "1.22.22"}
USER_CHECK = '''import os, pathlib, tempfile
assert os.getuid() == 1000
assert os.environ["HOME"] == "/home/user"
assert pathlib.Path.home() == pathlib.Path("/home/user")
for directory in ("/home/user", "/code", "/usr/local", "/usr/local/bin",
                  "/usr/local/lib", "/usr/local/share", "/usr/local/lib/node_modules",
                  "/usr/local/lib/python3.11/site-packages"):
    with tempfile.NamedTemporaryFile(prefix="controlled-smoke-", dir=directory) as handle:
        handle.write(b"writable")
        handle.flush()
print("CONTROLLED_USER_OK")
'''


def run(argv):
    result = subprocess.run(argv, check=True, capture_output=True, text=True,
                            timeout=25, stdin=subprocess.DEVNULL)
    return result.stdout.strip()


def main():
    assert os.geteuid() == 0, "build gate must start as root"
    assert sys.version_info[:3] == (3, 11, 18)
    for package, expected in (("pip", "26.3"), ("setuptools", "84.2.1"),
                              ("wheel", "0.48.0"), ("packaging", "26.3")):
        assert version(package) == expected, package
    assert sys.executable.startswith("/usr/local/")
    assert ssl.OPENSSL_VERSION and ssl.create_default_context().get_ca_certs()
    with sqlite3.connect(":memory:") as database:
        assert database.execute("select 6 * 7").fetchone() == (42,)
    assert ctypes.CDLL(None).printf
    with tempfile.TemporaryDirectory(prefix="proxcloud-controlled-") as directory:
        root = Path(directory)
        cpp = root / "hello.cpp"
        cpp.write_text('#include <iostream>\nint main(){std::cout << "CPP_OK"; return 0;}\n')
        run(["c++", str(cpp), "-o", str(root / "hello")])
        assert run([str(root / "hello")]) == "CPP_OK"
        c = root / "controlled_native.c"
        c.write_text('#include <Python.h>\n'
                     'static PyObject *answer(PyObject *s, PyObject *a){return PyLong_FromLong(42);}\n'
                     'static PyMethodDef methods[]={{"answer",answer,METH_NOARGS,NULL},{NULL,NULL,0,NULL}};\n'
                     'static struct PyModuleDef module={PyModuleDef_HEAD_INIT,"controlled_native",NULL,-1,methods};\n'
                     'PyMODINIT_FUNC PyInit_controlled_native(void){return PyModule_Create(&module);}\n')
        extension = root / ("controlled_native" + sysconfig.get_config_var("EXT_SUFFIX"))
        run(["cc", "-shared", "-fPIC", "-I" + sysconfig.get_path("include"), str(c), "-o", str(extension)])
        spec = importlib.util.spec_from_file_location("controlled_native", extension)
        native = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(native)
        assert native.answer() == 42
    for executable, expected_version in VERSIONS.items():
        assert run([executable, "--version"]) == expected_version, executable
    js = 'for(const m of ["fs","http","crypto"]) require(m); console.log(JSON.stringify({ok:true,node:process.version}));'
    assert json.loads(run(["node", "-e", js])) == {"ok": True, "node": "v22.23.5"}
    assert run(["git", "--version"]).startswith("git version ")
    assert run(["gh", "--version"]).splitlines()[0].split()[:3] == ["gh", "version", "2.103.3+proxcloud1"]
    run(["ssh", "-V"])
    user = pwd.getpwnam("user")
    assert user.pw_uid == 1000 and user.pw_dir == "/home/user" and user.pw_shell == "/bin/bash"
    sudoers = Path("/etc/sudoers.d/user")
    assert sudoers.read_text().strip() == "user ALL=(ALL) NOPASSWD: ALL"
    assert sudoers.stat().st_mode & 0o777 == 0o440
    assert run(["su", "-s", "/bin/bash", "user", "-c",
                "HOME=/home/user /usr/local/bin/python3 -c " + shlex.quote(USER_CHECK)]) == "CONTROLLED_USER_OK"
    print(MARKER)


if __name__ == "__main__":
    main()
