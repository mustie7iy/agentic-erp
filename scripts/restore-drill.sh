#!/usr/bin/env bash
# The restore drill (ADR-0040, T41e): restore the drill cell's database to
# a point in time as a side database, verify it with `erp-server dr
# verify`, measure the RPO and RTO, and append the run to
# <drill cell>/drills/restore.json (deploy/environments/path.json "drill"),
# which only grows; a pull request of its own merges it, and the promotion
# checks refuse a production promotion when the last passed AWS drill is
# older than 90 days.
#   1. mark: `erp-server dr mark` writes a marker into the drill tenant's
#      audit chain ("before"), the restore point is taken after it, and a
#      second marker ("after") is written after the point;
#   2. restore the database to the point, beside the cell's own;
#   3. verify: every check of `erp-server dr verify`, the "before" marker
#      present with the tenant's fingerprint as it was, "after" absent;
#   4. measure: the RPO is the restore point less the newest data the
#      restore kept (the "before" marker), the RTO the restore's start to
#      the verification's pass, which fails the drill past 30 minutes;
#      then the side database is removed (a removal that fails, too).
# The pods are labelled erp-restore-drill, which the erp-cell chart's
# NetworkPolicy of that name lets reach the databases in the VPC.
# RESTORE_PROVIDER picks where:
#   aws (staging): RESTORE_KIND=point-in-time restores the cell's DB
#     cluster RESTORE_CLUSTER with restore-db-cluster-to-point-in-time
#     (RESTORE_ARGS: further options, the subnet group, security groups,
#     instance class, as the cell's Terraform outputs give them);
#     RESTORE_KIND=dr-copies restores the newest copy in the DR region made
#     after the "before" marker with AWS Backup (RESTORE_ROLE_ARN,
#     RESTORE_METADATA: further restore metadata as JSON), its point the
#     copy's creation, into the DR VPC the cell's Terraform keeps for the
#     drill (RESTORE_DR_TASK: its restore_drill output as JSON). The
#     markers run as one-off pods of the cell's server image in
#     RESTORE_NAMESPACE (erp) with the migrations' database URL (secret
#     erp-migrate); KUBECTL may carry a --context. The verification runs
#     beside the side cluster: as a pod in the home region for a point in
#     time, and for the copies as a Fargate task in the DR region (T41e2),
#     its task definition registered for the run and removed after it, the
#     side cluster's master password from its RDS-managed secret
#     (PGPASSWORD), the DR region's RDS certificate authorities from the
#     file RESTORE_DR_CA (ERP_DR_DATABASE_CA), its report read from the log
#     group. DRILL_TENANT is the drill tenant.
#   local (weekly CI, a laptop): PostgreSQL 17 in Docker archiving its WAL,
#     a base backup, and a second container restored from both with
#     recovery_target_time; erp-server is built from this checkout
#     (ERP_SERVER names another), the drill tenant created.
# AWS, KUBECTL, DOCKER and ERP_SERVER name the commands,
# RESTORE_TIMEOUT a wait's limit in seconds (7200: a copy is hourly),
# RESTORE_POLL the seconds between looks (15), DRILL_BY who ran it.
#   scripts/restore-drill.sh https://<where the drill's log is kept>
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - "${1:?usage: restore-drill.sh RUN_URL}" <<'PY'
import base64, datetime, json, os, re, shlex, subprocess, sys, tempfile, time, urllib.parse, uuid

run_url = sys.argv[1]
env = os.environ.get
provider, kind = env("RESTORE_PROVIDER", "aws"), env("RESTORE_KIND", "point-in-time")
timeout, poll = int(env("RESTORE_TIMEOUT", "7200")), float(env("RESTORE_POLL", "15"))
root = "deploy/environments"
work = tempfile.mkdtemp(prefix="restore-drill-")
stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d%H%M%S")

def fail(msg):
    sys.exit("restore-drill: " + msg)

class Step(Exception):
    """A step that failed: the drill's result is failed."""

def sh(cmd, *args, env_extra=None, check=True):
    try:
        r = subprocess.run(shlex.split(cmd) + list(args), capture_output=True, text=True, env=dict(os.environ, **(env_extra or {})))
    except OSError as e:
        raise Step("%s: %s" % (cmd, e))
    if check and r.returncode != 0:
        raise Step("%s %s: %s" % (cmd, " ".join(args[:3]), (r.stderr or r.stdout).strip()[-400:]))
    return r

def when(text):
    return datetime.datetime.fromisoformat(text.replace("Z", "+00:00")).astimezone(datetime.timezone.utc)

def iso(t):
    return t.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")

def until(what, check):
    """Polls check until it returns a value, for at most the timeout."""
    deadline = time.monotonic() + timeout
    while True:
        got = check()
        if got:
            return got
        if time.monotonic() > deadline:
            raise Step("%s: not within %ds" % (what, timeout))
        time.sleep(poll)

def report(text):
    """The JSON object erp-server printed, whatever else the output holds."""
    start = text.find("{")
    if start < 0:
        raise Step("erp-server printed no report: %s" % text.strip()[-400:])
    return json.JSONDecoder().raw_decode(text[start:])[0]

# --- local: PostgreSQL in Docker, a base backup and the WAL archive ---

class Local:
    image = "postgres:17"

    def __init__(self):
        self.docker = env("DOCKER", "docker")
        self.prefix = "erp-restore-%d" % os.getpid()
        self.source_port, self.side_port = env("RESTORE_LOCAL_PORT", "55470"), env("RESTORE_LOCAL_SIDE_PORT", "55471")
        self.tenant = str(uuid.uuid4())
        self.server = env("ERP_SERVER")

    def url(self, port):
        return "postgres://postgres:dev@127.0.0.1:%s/postgres?sslmode=disable" % port

    def ready(self, name):
        try:
            until("%s ready" % name, lambda: sh(self.docker, "exec", name, "pg_isready", "-q", "-h", "127.0.0.1", check=False).returncode == 0)
        except Step as e:
            logs = sh(self.docker, "logs", "--tail", "20", name, check=False)
            raise Step("%s\n%s" % (e, (logs.stdout + logs.stderr).strip()))

    def sql(self, container, query):
        """psql inside the container: the host needs no client."""
        return sh(self.docker, "exec", self.prefix + "-" + container, "psql", "-U", "postgres", "-XAtc", query).stdout.strip()

    def erp(self, port, *args):
        r = sh(self.server, *args, env_extra={"DATABASE_URL": self.url(port), "ERP_ENV": "development"}, check=False)
        return r.returncode, r.stdout + r.stderr

    def prepare(self):
        if not self.server:
            self.server = os.path.join(work, "erp-server")
            sh("go", "build", "-o", self.server, "./cmd/erp-server")
        src = self.prefix + "-source"
        sh(self.docker, "volume", "create", self.prefix)
        sh(self.docker, "run", "-d", "--name", src, "-p", "127.0.0.1:%s:5432" % self.source_port, "-v", self.prefix + ":/wal",
           "-e", "POSTGRES_PASSWORD=dev", self.image, "-c", "wal_level=replica", "-c", "archive_mode=on",
           "-c", "archive_command=test ! -f /wal/archive/%f && cp %p /wal/archive/%f")
        self.ready(src)
        sh(self.docker, "exec", "-u", "root", src, "bash", "-c", "mkdir -p /wal/archive && chown postgres /wal /wal/archive")
        code, out = self.erp(self.source_port, "migrate")
        if code != 0:
            raise Step("migrate: " + out.strip()[-400:])
        self.sql("source", "INSERT INTO tenancy.tenant (id, name, region, group_currency, status) "
                 "VALUES ('%s', 'Restore drill', 'eu-west', 'EUR', 'active')" % self.tenant)
        sh(self.docker, "exec", "-u", "postgres", src, "pg_basebackup", "-D", "/wal/base", "-X", "fetch")

    def mark(self, name):
        code, out = self.erp(self.source_port, "dr", "mark", "-tenant", self.tenant, "-name", name)
        if code != 0:
            raise Step("mark %s: %s" % (name, out.strip()[-400:]))
        return report(out)

    def point(self):
        """The restore point, on the database's clock."""
        return when(self.sql("source", "SELECT to_json(now())#>>'{}'"))

    def restore(self, point):
        # The segment holding the "after" marker is archived, so recovery
        # reads past the point and stops there.
        segment = self.sql("source", "SELECT pg_walfile_name(pg_switch_wal())")
        until("WAL archived", lambda: self.sql("source", "SELECT coalesce(last_archived_wal, '') >= '%s' FROM pg_stat_archiver" % segment) == "t")
        side = self.prefix + "-side"
        sh(self.docker, "run", "-d", "--name", side, "-p", "127.0.0.1:%s:5432" % self.side_port, "-v", self.prefix + ":/wal",
           "--user", "postgres", "-e", "PGDATA=/var/lib/postgresql/data/pgdata", "--entrypoint", "bash", self.image, "-c",
           'cp -a /wal/base "$PGDATA" && chmod 700 "$PGDATA" && touch "$PGDATA/recovery.signal" && exec postgres '
           "-c restore_command='cp /wal/archive/%%f %%p' -c recovery_target_time='%s' -c recovery_target_action=promote" % point.strftime("%Y-%m-%d %H:%M:%S.%f+00"))
        self.ready(side)
        until("recovery ended", lambda: self.sql("side", "SELECT pg_is_in_recovery()") == "f")
        return point

    def verify(self, before, after):
        return self.erp(self.side_port, "dr", "verify", "-restored", before, "-lost", after)

    def cleanup(self):
        sh(self.docker, "rm", "-f", self.prefix + "-source", self.prefix + "-side", check=False)
        sh(self.docker, "volume", "rm", self.prefix, check=False)

# --- aws: a side cluster beside the cell's, erp-server as a one-off pod ---

class AWS:
    def __init__(self):
        self.aws, self.kubectl = env("AWS", "aws"), env("KUBECTL", "kubectl")
        self.ns = env("RESTORE_NAMESPACE", "erp")
        self.cluster = env("RESTORE_CLUSTER") or fail("RESTORE_CLUSTER names the cell's DB cluster")
        self.tenant = env("DRILL_TENANT") or fail("DRILL_TENANT names the drill tenant")
        self.side = "%s-drill-%s" % (self.cluster, stamp)
        self.region = None  # the side cluster's region; None: the default
        self.objects = []
        if kind == "dr-copies":
            self.dr = json.loads(env("RESTORE_DR_TASK") or fail("RESTORE_DR_TASK is the cell's restore_drill output (Terraform)"))
            self.ca = env("RESTORE_DR_CA") or fail("RESTORE_DR_CA names the DR region's RDS certificate authorities (PEM)")

    def json(self, *args):
        return json.loads(sh(self.aws, *args, "--output", "json").stdout or "null")

    def kube(self, *args, check=True):
        return sh(self.kubectl, "-n", self.ns, *args, check=check)

    def regional(self):
        return ["--region", self.region] if self.region else []

    def described(self):
        return self.json("rds", "describe-db-clusters", *self.regional(), "--db-cluster-identifier", self.side)["DBClusters"][0]

    def available(self):
        until("%s available" % self.side, lambda: self.described()["Status"] == "available")

    def prepare(self):
        self.image = self.kube("get", "deployment", "erp-server", "-o", "jsonpath={.spec.template.spec.containers[0].image}").stdout.strip()
        # The chart's erp-restore-drill NetworkPolicy selects the pods by
        # name and release: the cell's default deny lets nothing else out.
        self.release = self.kube("get", "deployment", "erp-server", "-o",
                                 "jsonpath={.metadata.labels.app\\.kubernetes\\.io/instance}").stdout.strip()
        if not self.release:
            raise Step("deployment erp-server has no app.kubernetes.io/instance label: the drill's pods would match no NetworkPolicy")
        raw = self.kube("get", "secret", "erp-migrate", "-o", "jsonpath={.data.database-url}").stdout.strip()
        self.source_url = base64.b64decode(raw).decode()

    def pod(self, name, args, secret, files=None):
        """Runs erp-server ARGS in a pod with DATABASE_URL from the secret; returns its exit code and log."""
        mounts = [{"name": "url", "mountPath": "/etc/erp/drill-db", "readOnly": True},
                  {"name": "rds", "mountPath": "/etc/erp/rds", "readOnly": True}]
        volumes = [{"name": "url", "secret": {"secretName": secret}}, {"name": "rds", "configMap": {"name": "erp-rds-ca"}}]
        if files:
            self.kube("create", "configmap", name, *["--from-file=%s" % f for f in files])
            self.objects.append(("configmap", name))
            mounts.append({"name": "markers", "mountPath": "/etc/erp/drill", "readOnly": True})
            volumes.append({"name": "markers", "configMap": {"name": name}})
        spec = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": {"app.kubernetes.io/name": "erp-restore-drill",
                                                              "app.kubernetes.io/instance": self.release}},
                "spec": {"restartPolicy": "Never", "automountServiceAccountToken": False,
                         "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
                         "containers": [{"name": "erp-server", "image": self.image, "args": args,
                                         "env": [{"name": "ERP_ENV", "value": "production"},
                                                 {"name": "DATABASE_URL", "value": "file:/etc/erp/drill-db/database-url"}],
                                         "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                                             "capabilities": {"drop": ["ALL"]}},
                                         "volumeMounts": mounts}],
                         "volumes": volumes}}
        path = os.path.join(work, name + ".json")
        json.dump(spec, open(path, "w"))
        self.kube("apply", "-f", path)
        self.objects.append(("pod", name))
        def done():
            phase = self.kube("get", "pod", name, "-o", "jsonpath={.status.phase}").stdout.strip()
            return phase if phase in ("Succeeded", "Failed") else None
        phase = until("pod %s done" % name, done)
        return (0 if phase == "Succeeded" else 1), self.kube("logs", name).stdout

    def mark(self, name):
        code, out = self.pod("restore-drill-%s-%s" % (stamp, name.split("-")[0]), ["dr", "mark", "-tenant", self.tenant, "-name", name], "erp-migrate")
        if code != 0:
            raise Step("mark %s: %s" % (name, out.strip()[-400:]))
        return report(out)

    def point(self):
        if kind == "dr-copies":
            return self.newest_copy()
        point = datetime.datetime.now(datetime.timezone.utc)
        until("point in time restorable", lambda: when(self.json("rds", "describe-db-clusters", "--db-cluster-identifier", self.cluster)
                                                         ["DBClusters"][0]["LatestRestorableTime"]) >= point)
        return point

    def restore(self, point):
        # Removed at the end whatever happens: a failed restore may leave one.
        self.objects.append(("cluster", self.side))
        if kind == "point-in-time":
            sh(self.aws, "rds", "restore-db-cluster-to-point-in-time", "--source-db-cluster-identifier", self.cluster,
               "--db-cluster-identifier", self.side, "--restore-to-time", iso(point), *shlex.split(env("RESTORE_ARGS", "")))
        else:
            self.from_copy()
        self.available()
        # The side cluster's own master secret, which RDS rotates and the
        # pod reads; the cell's is not touched.
        sh(self.aws, "rds", "modify-db-cluster", *self.regional(), "--db-cluster-identifier", self.side,
           "--manage-master-user-password", "--apply-immediately")
        until("%s master secret" % self.side, lambda: self.described().get("MasterUserSecret", {}).get("SecretStatus") == "active")
        self.available()
        return point

    def newest_copy(self):
        """Waits for a copy in the DR region made after the "before"
        marker; its creation is the restore point."""
        self.region, vault = self.dr["region"], self.dr["vault"]
        def newest():
            points = self.json("backup", "list-recovery-points-by-backup-vault", "--region", self.region, "--backup-vault-name", vault)
            done = [p for p in points.get("RecoveryPoints", []) if p.get("Status") == "COMPLETED" and when(p["CreationDate"]) > self.before_at]
            return max(done, key=lambda p: when(p["CreationDate"])) if done else None
        self.copy = until("a copy in %s made after the marker" % self.region, newest)
        return when(self.copy["CreationDate"])

    def from_copy(self):
        # Into the DR VPC, where the verification task runs; no peering.
        metadata = dict(json.loads(env("RESTORE_METADATA", "{}")), DBClusterIdentifier=self.side,
                        DBSubnetGroupName=self.dr["db_subnet_group"], VpcSecurityGroupIds=json.dumps([self.dr["db_security_group"]]))
        job = self.json("backup", "start-restore-job", "--region", self.region, "--recovery-point-arn", self.copy["RecoveryPointArn"],
                        "--iam-role-arn", env("RESTORE_ROLE_ARN") or fail("RESTORE_ROLE_ARN names AWS Backup's restore role"),
                        "--metadata", json.dumps(metadata))["RestoreJobId"]
        def finished():
            status = self.json("backup", "describe-restore-job", "--region", self.region, "--restore-job-id", job)["Status"]
            if status in ("ABORTED", "FAILED"):
                raise Step("restore job %s: %s" % (job, status))
            return status == "COMPLETED"
        until("restore job %s" % job, finished)

    def verify(self, before, after):
        if kind == "dr-copies":
            return self.task(before, after)
        c = self.described()
        secret = json.loads(self.json("secretsmanager", "get-secret-value", *self.regional(), "--secret-id",
                                      c["MasterUserSecret"]["SecretArn"])["SecretString"])
        u = urllib.parse.urlsplit(self.source_url)
        netloc = "%s:%s@%s:%s" % (urllib.parse.quote(secret["username"], safe=""), urllib.parse.quote(secret["password"], safe=""),
                                  c["Endpoint"], c.get("Port", 5432))
        name = "restore-drill-%s-db" % stamp
        url = os.path.join(work, "database-url")
        with open(url, "w") as f:
            f.write(urllib.parse.urlunsplit(u._replace(netloc=netloc)))
        self.kube("create", "secret", "generic", name, "--from-file=database-url=" + url)
        self.objects.append(("secret", name))
        return self.pod("restore-drill-%s-verify" % stamp, ["dr", "verify", "-restored", "/etc/erp/drill/before.json",
                                                            "-lost", "/etc/erp/drill/after.json"], name, [before, after])

    def task(self, before, after):
        """Runs erp-server dr verify as a Fargate task in the DR region,
        beside the side cluster; returns its exit code and log."""
        c, t = self.described(), self.dr
        u = urllib.parse.urlsplit(self.source_url)
        query = urllib.parse.urlencode([(k, v) for k, v in urllib.parse.parse_qsl(u.query) if k != "sslrootcert"])
        netloc = "%s@%s:%s" % (urllib.parse.quote(c["MasterUsername"], safe=""), c["Endpoint"], c.get("Port", 5432))
        # The registry replicates the images to the DR region (ADR-0040).
        image = re.sub(r"\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com", ".dkr.ecr.%s.amazonaws.com" % self.region, self.image, count=1)
        environment = {"ERP_ENV": "production", "DATABASE_URL": urllib.parse.urlunsplit(u._replace(netloc=netloc, query=query)),
                       "ERP_DR_DATABASE_CA": open(self.ca).read(), "ERP_DR_RESTORED": open(before).read(), "ERP_DR_LOST": open(after).read()}
        container = {"name": "erp-server", "image": image, "essential": True, "user": "65532", "readonlyRootFilesystem": True,
                     "command": ["dr", "verify", "-restored", "env:ERP_DR_RESTORED", "-lost", "env:ERP_DR_LOST"],
                     "environment": [{"name": k, "value": v} for k, v in environment.items()],
                     "secrets": [{"name": "PGPASSWORD", "valueFrom": c["MasterUserSecret"]["SecretArn"] + ":password::"}],
                     "mountPoints": [{"sourceVolume": "tmp", "containerPath": "/tmp"}],
                     "linuxParameters": {"capabilities": {"drop": ["ALL"]}},
                     "logConfiguration": {"logDriver": "awslogs", "options": {"awslogs-group": t["log_group"], "awslogs-region": self.region,
                                                                             "awslogs-stream-prefix": "verify"}}}
        ecs = ["--region", self.region]
        definition = self.json("ecs", "register-task-definition", *ecs, "--family", "%s-drill-%s" % (self.cluster, stamp),
                               "--requires-compatibilities", "FARGATE", "--network-mode", "awsvpc", "--cpu", "512", "--memory", "1024",
                               "--execution-role-arn", t["execution_role_arn"], "--container-definitions", json.dumps([container]),
                               "--volumes", json.dumps([{"name": "tmp"}]))["taskDefinition"]["taskDefinitionArn"]
        self.objects.append(("taskdef", definition))
        ran = self.json("ecs", "run-task", *ecs, "--cluster", t["cluster"], "--launch-type", "FARGATE", "--task-definition", definition,
                        "--network-configuration", json.dumps({"awsvpcConfiguration": {"subnets": t["subnets"], "securityGroups": [t["security_group"]],
                                                                                       "assignPublicIp": "DISABLED"}}))
        if not ran.get("tasks"):
            raise Step("run-task: %s" % json.dumps(ran.get("failures")))
        arn = ran["tasks"][0]["taskArn"]
        self.objects.append(("task", arn))
        def stopped():
            d = self.json("ecs", "describe-tasks", *ecs, "--cluster", t["cluster"], "--tasks", arn)["tasks"][0]
            return d if d.get("lastStatus") == "STOPPED" else None
        done = until("task %s stopped" % arn, stopped)
        events = self.json("logs", "get-log-events", *ecs, "--log-group-name", t["log_group"], "--log-stream-name",
                           "verify/erp-server/" + arn.rsplit("/", 1)[1], "--start-from-head")["events"]
        out = "\n".join(e["message"] for e in events)
        code = done["containers"][0].get("exitCode")
        if code is None:  # never ran: the image, the secret, the network
            return 1, "%s\n%s" % (out, done.get("stoppedReason", ""))
        return (0 if code == 0 else 1), out

    def cleanup(self):
        """Removes what the drill made; a removal that fails is reported
        and fails the drill: a side cluster left running costs money."""
        failures = []
        for what, name in reversed(self.objects):
            if what == "cluster":
                r = sh(self.aws, "rds", "delete-db-cluster", *self.regional(), "--db-cluster-identifier", name, "--skip-final-snapshot", check=False)
                # Not found: a restore that failed before making it.
                if r.returncode != 0 and "DBClusterNotFoundFault" not in r.stderr:
                    failures.append("delete the side cluster %s by hand: %s" % (name, r.stderr.strip()[-300:]))
            elif what == "task":
                if sh(self.aws, "ecs", "stop-task", *self.regional(), "--cluster", self.dr["cluster"], "--task", name, check=False).returncode != 0:
                    failures.append("stop the task %s by hand" % name)
            elif what == "taskdef":
                gone = [sh(self.aws, "ecs", verb, *self.regional(), flag, name, check=False).returncode == 0
                        for verb, flag in (("deregister-task-definition", "--task-definition"), ("delete-task-definitions", "--task-definitions"))]
                if not all(gone):
                    failures.append("delete the task definition %s by hand" % name)
            elif self.kube("delete", what, name, "--ignore-not-found", check=False).returncode != 0:
                failures.append("delete %s %s in %s by hand" % (what, name, self.ns))
        if failures:
            raise Step("; ".join(failures))

if provider not in ("aws", "local"):
    fail("RESTORE_PROVIDER %r is neither aws nor local" % provider)
if kind not in ("point-in-time", "dr-copies") or (provider == "local" and kind != "point-in-time"):
    fail("RESTORE_KIND %r: point-in-time, or dr-copies with aws" % kind)
if not run_url.startswith("https://"):
    fail("RUN_URL %r is not an https:// address of the drill's log" % run_url)
cell = json.load(open(root + "/path.json"))["drill"]
record = "%s/%s/drills/restore.json" % (root, cell)
drill = Local() if provider == "local" else AWS()
started = datetime.datetime.now(datetime.timezone.utc)
result, point, rpo, rto = "failed", None, None, None
try:
    drill.prepare()
    files = {}
    def mark(name):
        m = drill.mark("%s-%s" % (name, stamp))
        files[name] = os.path.join(work, name + ".json")
        json.dump(m, open(files[name], "w"))
        return when(m["at"])
    drill.before_at = mark("before")
    time.sleep(1)  # the point strictly past the "before" marker
    point = drill.point()
    time.sleep(1)  # the "after" marker strictly past the point
    mark("after")
    restore_started = time.monotonic()
    point = drill.restore(point)
    code, out = drill.verify(files["before"], files["after"])
    verified = report(out)
    if code != 0 or verified.get("problems"):
        raise Step("the verification failed: %s" % "; ".join(verified.get("problems") or [out.strip()[-400:]]))
    rto = round(time.monotonic() - restore_started)
    rpo = round((point - when(verified["restored_marker"]["at"])).total_seconds(), 3)
    if rto > 1800:
        raise Step("the restore took %ds, past the RTO of 30 minutes (01 §1.3)" % rto)
    result = "passed"
except Exception as e:  # any failure is a failed run, recorded as such
    print("restore-drill: %s" % e, file=sys.stderr)
finally:
    try:
        drill.cleanup()
    except Step as e:
        result = "failed"
        print("restore-drill: cleaning up: %s" % e, file=sys.stderr)

runs = json.load(open(record)) if os.path.exists(record) else []
runs.append({"kind": kind, "provider": provider, "by": env("DRILL_BY") or env("GITHUB_ACTOR") or env("USER", "unknown"),
             "started": iso(started), "restore_point": iso(point) if point else None,
             "rpo_seconds": rpo, "rto_seconds": rto, "result": result, "run": run_url})
os.makedirs(os.path.dirname(record), exist_ok=True)
with open(record, "w") as f:
    json.dump(runs, f, indent=1)
    f.write("\n")
print("restore-drill: %s %s %s, RPO %ss, RTO %ss; recorded in %s" % (provider, kind, result, rpo, rto, record))
sys.exit(0 if result == "passed" else 1)
PY
