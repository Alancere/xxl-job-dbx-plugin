// End-to-end smoke test for the packaged XXL-JOB plugin:
//
//   node plugins/xxl-job/smoke.mjs
//
// 1. runs the sidecar unit/integration tests,
// 2. builds an unsigned .dbxp candidate,
// 3. re-opens the package and re-verifies every checksum, the entry layout,
//    and the executable mode the DBX installer relies on,
// 4. extracts the packaged binary, starts a mock XXL-JOB admin console, and
//    drives the real sidecar over the documented JSON-Lines protocol.
import { mkdtemp, chmod, readFile, rm, stat, writeFile } from "node:fs/promises";
import { readFileSync } from "node:fs";
import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { inflateRawSync } from "node:zlib";
import { fileURLToPath } from "node:url";

const root = path.dirname(fileURLToPath(import.meta.url));
const target = process.env.DBX_PLUGIN_TARGET || `${process.platform === "darwin" ? "darwin" : process.platform === "win32" ? "windows" : "linux"}-${process.arch === "arm64" ? "arm64" : "x64"}`;
const executableName = target.startsWith("windows-") ? "dbx-xxljob.exe" : "dbx-xxljob";
const manifest = JSON.parse(await readFile(path.join(root, "manifest.json"), "utf8"));

step("run sidecar tests");
run("go", ["test", "./..."], path.join(root, "backend"));

step("build .dbxp candidate");
run(process.execPath, ["package.mjs"], root);

const packagePath = path.join(root, "dist", `${manifest.id}-${manifest.version}-${target}.dbxp`);
const entries = readZip(packagePath);
step("verify package layout");
const names = entries.map((entry) => entry.name);
assert(names.includes("manifest.json"), "manifest.json is missing");
assert(names.includes("checksums.json"), "checksums.json is missing");
assert(names.includes(`bin/${target}/${executableName}`), "sidecar binary is missing");
assert(names.includes("ui/index.html"), "workbench UI is missing");
assert(names.includes("LICENSE"), "LICENSE is missing from the package");
assert(!names.some((name) => name.endsWith("/")), "package must not contain directory entries");
assert(new Set(names).size === names.length, "package contains duplicate entries");

const stagedManifest = JSON.parse(entries.find((entry) => entry.name === "manifest.json").data.toString("utf8"));
assert(stagedManifest.entrypoints.backend.executable === `bin/${target}/${executableName}`, "manifest backend entrypoint is not platform specific");
assert(stagedManifest.version === manifest.version, "staged manifest version does not match the source manifest");

const checksums = JSON.parse(entries.find((entry) => entry.name === "checksums.json").data.toString("utf8"));
assert(checksums.algorithm === "sha256", "unexpected checksum algorithm");
const covered = Object.keys(checksums.files).sort();
const actual = names.filter((name) => name !== "checksums.json").sort();
assert(JSON.stringify(covered) === JSON.stringify(actual), `checksums cover ${covered.length} files but the package has ${actual.length}`);
for (const entry of entries) {
  if (entry.name === "checksums.json") continue;
  const digest = createHash("sha256").update(entry.data).digest("hex");
  assert(digest === checksums.files[entry.name], `checksum mismatch for ${entry.name}`);
}
const binaryEntry = entries.find((entry) => entry.name === `bin/${target}/${executableName}`);
assert((binaryEntry.mode & 0o111) !== 0, "sidecar binary must be packaged executable");
const packageSize = (await stat(packagePath)).size;
console.log(`  ${names.length} entries, ${packageSize} bytes verified`);

step("start mock XXL-JOB admin");
const admin = await startMockAdmin();
console.log(`  listening on ${admin.url}/xxl-job-admin`);

step("drive the packaged sidecar");
const workdir = await mkdtemp(path.join(os.tmpdir(), "dbx-xxljob-smoke-"));
try {
  const binaryPath = path.join(workdir, executableName);
  await writeFile(binaryPath, binaryEntry.data);
  await chmod(binaryPath, 0o755);
  const sidecar = spawn(binaryPath, [], { stdio: ["pipe", "pipe", "pipe"] });
  let stderr = "";
  sidecar.stderr.on("data", (chunk) => {
    stderr += chunk.toString();
  });
  const client = jsonlClient(sidecar);
  const lifecycle = {
    provider: { id: "alancere.xxljob.connection", databaseType: "xxljob" },
    connection: {
      id: "smoke-connection",
      name: "Smoke XXL-JOB",
      host: "127.0.0.1",
      port: admin.port,
      username: "admin",
      password: "123456",
      external_config: { scheme: "http", context_path: "/xxl-job-admin", connect_timeout_secs: 10 },
    },
    runtime: { host: "127.0.0.1", port: admin.port },
  };

  const handshake = await client.call("plugin/initialize", { host: { protocolVersions: [1] } });
  assert(handshake.result?.protocolVersion === 1, "handshake did not negotiate protocol v1");
  assert(handshake.result?.plugin?.id === "alancere.xxljob", "unexpected plugin identity");

  const test = await client.call("connection/test", lifecycle);
  assert(test.result?.success === true, `connection/test failed: ${JSON.stringify(test)}`);
  console.log(`  connection/test: ${test.result.message}`);

  const connect = await client.call("connection/connect", lifecycle);
  assert(connect.result?.success === true, `connection/connect failed: ${JSON.stringify(connect)}`);

  const jobs = await client.call("xxljob/jobs", { connectionId: "smoke-connection", groupId: 0, length: 20 });
  assert(jobs.result?.total === 2, `expected 2 jobs, got ${JSON.stringify(jobs.result)}`);
  console.log(`  xxljob/jobs: ${jobs.result.jobs.map((job) => job.jobDesc).join(", ")}`);

  const dashboard = await client.call("xxljob/dashboard", { connectionId: "smoke-connection", days: 7 });
  assert(dashboard.result?.stats?.jobs === 2, "dashboard counters are wrong");

  const trigger = await client.call("xxljob/job/trigger", { connectionId: "smoke-connection", id: 1, executorParam: "from=smoke" });
  assert(trigger.result?.success === true, `trigger failed: ${JSON.stringify(trigger)}`);

  const logs = await client.call("xxljob/logs", { connectionId: "smoke-connection", groupId: -1, jobId: 1, length: 10 });
  assert(logs.result?.total === 1, "job log page is empty");

  const detail = await client.call("xxljob/log", { connectionId: "smoke-connection", logId: 11, fromLineNum: 1 });
  assert(String(detail.result?.logContent || "").includes("mock log line"), "log detail has no content");

  const stop = await client.call("xxljob/job/stop", { connectionId: "smoke-connection", id: 1 });
  assert(stop.result?.success === true, "stop failed");
  const start = await client.call("xxljob/job/start", { connectionId: "smoke-connection", id: 1 });
  assert(start.result?.success === true, "start failed");

  const groups = await client.call("xxljob/groups", { connectionId: "smoke-connection" });
  assert(groups.result?.groups?.length === 2, "executor groups are wrong");

  const missing = await client.call("xxljob/jobs", { connectionId: "unknown" });
  assert(missing.error?.code === -32010, `expected a no-session error, got ${JSON.stringify(missing)}`);

  const disconnect = await client.call("connection/disconnect", { connectionId: "smoke-connection" });
  assert(disconnect.result?.success === true, "disconnect failed");

  client.close();
  await new Promise((resolve) => sidecar.once("exit", resolve));
  assert(!stderr.includes("panic"), `sidecar wrote a panic to stderr:\n${stderr}`);
  console.log("  sidecar stopped cleanly");
} finally {
  await rm(workdir, { recursive: true, force: true });
  await admin.close();
}

console.log("\nXXL-JOB plugin smoke test passed.");

// --- helpers ---------------------------------------------------------------

function step(label) {
  console.log(`\n▶ ${label}`);
}

function run(command, args, cwd) {
  const result = spawnSync(command, args, { cwd, stdio: "inherit", env: process.env });
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(" ")} failed with exit code ${result.status}`);
  }
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function readZip(file) {
  return parseZip(readFileSync(file));
}

function parseZip(buffer) {
  const eocd = findEndOfCentralDirectory(buffer);
  const count = buffer.readUInt16LE(eocd + 10);
  let offset = buffer.readUInt32LE(eocd + 16);
  const entries = [];
  for (let index = 0; index < count; index += 1) {
    if (buffer.readUInt32LE(offset) !== 0x02014b50) throw new Error("invalid central directory entry");
    const method = buffer.readUInt16LE(offset + 10);
    const compressedSize = buffer.readUInt32LE(offset + 20);
    const uncompressedSize = buffer.readUInt32LE(offset + 24);
    const nameLength = buffer.readUInt16LE(offset + 28);
    const extraLength = buffer.readUInt16LE(offset + 30);
    const commentLength = buffer.readUInt16LE(offset + 32);
    const externalAttributes = buffer.readUInt32LE(offset + 38);
    const localOffset = buffer.readUInt32LE(offset + 42);
    const name = buffer.toString("utf8", offset + 46, offset + 46 + nameLength);
    const localNameLength = buffer.readUInt16LE(localOffset + 26);
    const localExtraLength = buffer.readUInt16LE(localOffset + 28);
    const start = localOffset + 30 + localNameLength + localExtraLength;
    const raw = buffer.subarray(start, start + compressedSize);
    const data = method === 8 ? inflateRawSync(raw) : Buffer.from(raw);
    if (data.length !== uncompressedSize) throw new Error(`size mismatch for ${name}`);
    entries.push({ name, data, mode: externalAttributes >>> 16 });
    offset += 46 + nameLength + extraLength + commentLength;
  }
  return entries;
}

function findEndOfCentralDirectory(buffer) {
  for (let offset = buffer.length - 22; offset >= 0; offset -= 1) {
    if (buffer.readUInt32LE(offset) === 0x06054b50) return offset;
  }
  throw new Error("not a ZIP archive");
}

function jsonlClient(child) {
  let buffer = "";
  let sequence = 0;
  const pending = new Map();
  child.stdout.on("data", (chunk) => {
    buffer += chunk.toString();
    let newline = buffer.indexOf("\n");
    while (newline >= 0) {
      const line = buffer.slice(0, newline).trim();
      buffer = buffer.slice(newline + 1);
      newline = buffer.indexOf("\n");
      if (!line) continue;
      let message;
      try {
        message = JSON.parse(line);
      } catch (error) {
        continue;
      }
      const resolver = pending.get(String(message.id));
      if (resolver) {
        pending.delete(String(message.id));
        resolver(message);
      }
    }
  });
  return {
    call(method, params) {
      sequence += 1;
      const id = sequence;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          pending.delete(String(id));
          reject(new Error(`${method} timed out`));
        }, 20000);
        pending.set(String(id), (message) => {
          clearTimeout(timer);
          resolve(message);
        });
        child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
      });
    },
    close() {
      child.stdin.end();
    },
  };
}

// Spring MVC binds the paged controllers' filters as primitive ints, so a
// missing jobGroup produces this message instead of a page envelope.
function primitiveIntError(name) {
  return `java.lang.IllegalStateException: Optional int parameter '${name}' is present but cannot be translated into a null value due to being declared as a primitive type. Consider declaring it as object wrapper for the corresponding primitive type.`;
}

function startMockAdmin() {
  const server = createServer((request, response) => {
    const url = new URL(request.url, "http://127.0.0.1");
    const pathname = url.pathname;
    if (pathname === "/xxl-job-admin/login") {
      let body = "";
      request.on("data", (chunk) => {
        body += chunk;
      });
      request.on("end", () => {
        const form = new URLSearchParams(body);
        if (form.get("userName") === "admin" && form.get("password") === "123456") {
          response.writeHead(302, { "Set-Cookie": "XXL_JOB_LOGIN_IDENTITY=smoke; Path=/", Location: "/xxl-job-admin/" });
          response.end();
        } else {
          response.writeHead(200, { "Content-Type": "application/json" });
          response.end(JSON.stringify({ code: 500, msg: "账号或密码错误" }));
        }
      });
      return;
    }
    request.resume();
    if (pathname === "/xxl-job-admin/toLogin") {
      response.writeHead(200, { "Content-Type": "text/html; charset=UTF-8" });
      response.end("<html><title>XXL-JOB</title><body>login</body></html>");
      return;
    }
    if (!String(request.headers.cookie || "").includes("XXL_JOB_LOGIN_IDENTITY=smoke")) {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ code: 500, msg: "登录已过期" }));
      return;
    }
    response.writeHead(200, { "Content-Type": "application/json" });
    switch (pathname) {
      case "/xxl-job-admin/jobgroup/pageList":
        response.end(
          JSON.stringify({
            recordsTotal: 2,
            recordsFiltered: 2,
            data: [
              { id: 1, appname: "xxl-job-executor-sample", title: "示例执行器", addressType: 0, addressList: "", registryList: ["http://127.0.0.1:9999/"] },
              { id: 2, appname: "billing-executor", title: "计费执行器", addressType: 1, addressList: "http://10.0.0.2:9999/", registryList: [] },
            ],
          }),
        );
        return;
      case "/xxl-job-admin/jobinfo/pageList": {
        // 2.4.1 declares jobGroup as a primitive int, so a request without it
        // gets this envelope (HTTP 200) instead of a page.
        if (!url.searchParams.has("jobGroup")) {
          response.end(JSON.stringify({ code: 500, msg: primitiveIntError("jobGroup"), content: null }));
          return;
        }
        const jobs = [
          { id: 1, jobGroup: 1, jobDesc: "示例任务", author: "XXL", scheduleType: "CRON", scheduleConf: "0 0 0 * * ?", glueType: "BEAN", executorHandler: "demoJobHandler", executorRouteStrategy: "ROUND", triggerStatus: 1 },
          { id: 2, jobGroup: 2, jobDesc: "账单核对", author: "alice", scheduleType: "FIX_RATE", scheduleConf: "30", glueType: "BEAN", executorHandler: "billCheck", executorRouteStrategy: "FIRST", triggerStatus: 0 },
        ];
        const group = Number(url.searchParams.get("jobGroup") || -1);
        const filtered = group > 0 ? jobs.filter((job) => job.jobGroup === group) : jobs;
        response.end(JSON.stringify({ recordsTotal: filtered.length, recordsFiltered: filtered.length, data: filtered }));
        return;
      }
      case "/xxl-job-admin/jobinfo/nextTriggerTime":
        response.end(JSON.stringify({ code: 200, msg: null, content: ["2026-10-22 00:00:00"] }));
        return;
      case "/xxl-job-admin/joblog/pageList":
        if (!url.searchParams.has("jobGroup")) {
          response.end(JSON.stringify({ code: 500, msg: primitiveIntError("jobGroup"), content: null }));
          return;
        }
        response.end(
          JSON.stringify({
            recordsTotal: 1,
            recordsFiltered: 1,
            data: [{ id: 11, jobGroup: 1, jobId: 1, jobDesc: "示例任务", executorAddress: "http://127.0.0.1:9999/", executorHandler: "demoJobHandler", triggerTime: 1700000000000, triggerCode: 200, handleCode: 200 }],
          }),
        );
        return;
      case "/xxl-job-admin/joblog/logDetailCat":
        response.end(JSON.stringify({ code: 200, msg: null, content: { fromLineNum: 1, toLineNum: 2, logContent: "mock log line\ncomplete\n", end: true } }));
        return;
      case "/xxl-job-admin/chartInfo":
        response.end(JSON.stringify({ code: 200, msg: null, content: { triggerDayList: ["2026-10-20"], triggerDayCount: [3], triggerDayCountSuc: [2], triggerDayCountFail: [1], triggerDayCountRunning: [0] } }));
        return;
      case "/xxl-job-admin/jobcode":
        response.writeHead(200, { "Content-Type": "text/html; charset=UTF-8" });
        response.end('<html><textarea id="glueSource" name="glueSource">echo smoke</textarea></html>');
        return;
      default:
        if (["/xxl-job-admin/jobinfo/start", "/xxl-job-admin/jobinfo/stop", "/xxl-job-admin/jobinfo/trigger", "/xxl-job-admin/jobinfo/add", "/xxl-job-admin/jobinfo/update", "/xxl-job-admin/jobinfo/remove", "/xxl-job-admin/joblog/logKill", "/xxl-job-admin/jobcode/save"].includes(pathname)) {
          response.end(JSON.stringify({ code: 200, msg: null, content: null }));
          return;
        }
        response.end(JSON.stringify({ code: 500, msg: "endpoint not modelled by the smoke mock" }));
    }
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      resolve({
        port,
        url: `http://127.0.0.1:${port}`,
        close: () => new Promise((done) => server.close(done)),
      });
    });
  });
}
