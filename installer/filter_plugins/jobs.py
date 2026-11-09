"""Read-only deployment checks that must run before stage 40 applies any Secret."""
from decimal import Decimal
import re


_DURATION_PART = re.compile(r"(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)")
_DURATION_UNITS = {"ns": Decimal("0.000000001"), "us": Decimal("0.000001"), "µs": Decimal("0.000001"),
                   "μs": Decimal("0.000001"), "ms": Decimal("0.001"), "s": Decimal(1), "m": Decimal(60),
                   "h": Decimal(3600)}


def _seconds(value):
    """Parse Go's duration unit syntax for positive bounded worker settings (not floats)."""
    if not isinstance(value, str) or not value or len(value) > 128:
        return None
    text = value[1:] if value.startswith("+") else value
    total, position = Decimal(0), 0
    for part in _DURATION_PART.finditer(text):
        if part.start() != position:
            return None
        total += Decimal(part[1]) * _DURATION_UNITS[part[2]]
        position = part.end()
    return total if position == len(text) and position else None


def _integer(value, low, high=None):
    if isinstance(value, bool) or not re.fullmatch(r"[0-9]+", str(value)):
        return False
    return int(value) >= low and (high is None or int(value) <= high)


def jobs_config_problem(config, release=""):
    """Return a human-readable failure, or empty when safe to render/apply the platform."""
    jobs, database = config.get("jobs", {}), config.get("database", {})
    if not _integer(jobs.get("worker_concurrency"), 1, 16):
        return "jobs.worker_concurrency must be an integer in 1..16"
    for key, low, high in (("poll_interval", Decimal("0.1"), 60), ("lease_duration", Decimal(10), 600)):
        seconds = _seconds(jobs.get(key))
        if seconds is None or not low <= seconds <= high:
            return "jobs.%s must be a Go duration in %s..%ss" % (key, low, high)
    for key in ("scheduler_enabled", "debug_triggers"):
        if not isinstance(jobs.get(key), bool):
            return "jobs.%s must be a YAML boolean" % key
    if not _integer(database.get("instances"), 1):
        return "database.instances must be a positive integer"
    if not isinstance(database.get("status_read_replica"), bool):
        return "database.status_read_replica must be a YAML boolean"
    if database["status_read_replica"] and int(database["instances"]) < 2:
        return "database.status_read_replica requires database.instances >= 2 before creating a reader Secret"
    if not config.get("images", {}).get("build", True):
        version = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)(?:\+[^\s]+)?", str(release))
        if not version or tuple(map(int, version.groups())) < (0, 4, 0):
            return "images.build=false requires a published broker-free release >= v0.4.0; use images.build=true before publication"
    return ""


def legacy_broker_problem(application, statefulsets, brokers):
    """Refuse ambiguous Argo sources or a legacy broker not protected by the CR retention gate."""
    spec = (application or {}).get("spec", {})
    if spec.get("sources"):
        return "existing multi-source Argo Application requires a separate manual broker retention review"
    helm = spec.get("source", {}).get("helm", {})
    if any(helm.get(key) for key in ("values", "valueFiles", "parameters", "fileParameters")):
        return "existing Argo helm values/valueFiles/parameters/fileParameters require a separate manual broker retention review"
    if helm.get("valuesObject", {}).get("rabbitmq", {}).get("enabled", False):
        return "existing Bitnami RabbitMQ subchart requires a separate manual child-resource retention plan"
    identities = {(b["metadata"]["name"], b["metadata"]["uid"]) for b in brokers if b.get("metadata", {}).get("uid")}
    for workload in statefulsets:
        metadata = workload.get("metadata", {})
        labels = metadata.get("labels", {})
        name = metadata.get("name", "")
        rabbit = "rabbit" in name.lower() or any("rabbit" in str(v).lower() for v in labels.values())
        if not rabbit:
            continue
        owned = any(owner.get("kind") == "RabbitmqCluster" and (owner.get("name"), owner.get("uid")) in identities
                    for owner in metadata.get("ownerReferences", []))
        if not owned:
            return "existing RabbitMQ StatefulSet %s is not owned by a discovered retained RabbitmqCluster; manual retention required" % name
    return ""


class FilterModule:
    def filters(self):
        return {"pc_jobs_config_problem": jobs_config_problem, "pc_legacy_broker_problem": legacy_broker_problem}
