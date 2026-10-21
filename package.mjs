// Builds one unsigned, platform-specific .dbxp candidate for the XXL-JOB
// plugin: compile the Go sidecar, stage the manifest/assets/UI, and write the
// ZIP container with checksums.json exactly as the DBX packager does.
//
//   node plugins/xxl-job/package.mjs
//   DBX_PLUGIN_TARGET=linux-x64 node plugins/xxl-job/package.mjs   # cross build
//
// Repository signing is a separate step (`dbx-plugin-packager sign`), so this
// script refuses to run while a signing key is present in the environment.
import { mkdir, mkdtemp, readFile, readdir, rm, stat, writeFile, cp } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { deflateRawSync } from "node:zlib";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const root = path.dirname(fileURLToPath(import.meta.url));
const manifest = JSON.parse(await readFile(path.join(root, "manifest.json"), "utf8"));
const dist = process.env.DBX_PLUGIN_OUTPUT_DIR ? path.resolve(process.env.DBX_PLUGIN_OUTPUT_DIR) : path.join(root, "dist");
const target = process.env.DBX_PLUGIN_TARGET || platformTarget();
const executableName = target.startsWith("windows-") ? "dbx-xxljob.exe" : "dbx-xxljob";
const output = path.join(dist, `${manifest.id}-${manifest.version}-${target}.dbxp`);
const artifactMetadata = output.replace(/\.dbxp$/, ".artifact.json");

const CRC_TABLE = (() => {
  const table = new Int32Array(256);
  for (let index = 0; index < 256; index += 1) {
    let value = index;
    for (let bit = 0; bit < 8; bit += 1) value = value & 1 ? (value >>> 1) ^ 0xedb88320 : value >>> 1;
    table[index] = value;
  }
  return table;
})();

async function main() {
  if (process.env.DBX_PLUGIN_SIGNING_KEY || process.env.DBX_PLUGIN_SIGNING_KEY_ID) {
    throw new Error("package.mjs only builds unsigned review candidates; sign the reviewed candidate with dbx-plugin-packager");
  }
  const buildRoot = await mkdtemp(path.join(os.tmpdir(), "dbx-xxljob-package-"));
  const stage = path.join(buildRoot, "stage");
  try {
    await mkdir(path.join(stage, "bin", target), { recursive: true });
    await mkdir(dist, { recursive: true });
    buildSidecar(path.join(stage, "bin", target, executableName));
    await cp(path.join(root, "assets"), path.join(stage, "assets"), { recursive: true });
    await cp(path.join(root, "ui"), path.join(stage, "ui"), { recursive: true });

    const staged = structuredClone(manifest);
    staged.entrypoints = { ...staged.entrypoints, backend: { executable: `bin/${target}/${executableName}` } };
    await writeFile(path.join(stage, "manifest.json"), `${JSON.stringify(staged, null, 2)}\n`);

    const files = await collectFiles(stage);
    const checksums = {
      algorithm: "sha256",
      files: Object.fromEntries(files.map((file) => [file.name, sha256(file.data)])),
    };
    files.push({ name: "checksums.json", data: Buffer.from(JSON.stringify(checksums, null, 2), "utf8"), mode: 0o644 });

    const packageBytes = writeZip(files);
    await writeFile(output, packageBytes);
    const artifact = {
      target,
      url: path.basename(output),
      sha256: sha256(packageBytes),
      size: packageBytes.length,
    };
    await writeFile(artifactMetadata, `${JSON.stringify(artifact, null, 2)}\n`);
    console.log(`Built unsigned candidate ${output}`);
    console.log(`  target ${target}, ${files.length} files, ${packageBytes.length} bytes`);
  } finally {
    await rm(buildRoot, { recursive: true, force: true });
  }
}

function buildSidecar(destination) {
  const [goos, goarch] = goPlatform(target);
  const result = spawnSync(
    "go",
    ["build", "-trimpath", "-ldflags", `-s -w -X main.buildVersion=${manifest.version}`, "-o", destination, "."],
    {
      cwd: path.join(root, "backend"),
      stdio: "inherit",
      env: { ...process.env, GOOS: goos, GOARCH: goarch, CGO_ENABLED: "0" },
    },
  );
  if (result.status !== 0) throw new Error(`go build failed for ${target}`);
}

async function collectFiles(directory, prefix = "") {
  const entries = [];
  const children = await readdir(directory, { withFileTypes: true });
  for (const child of children.sort((left, right) => left.name.localeCompare(right.name))) {
    const absolute = path.join(directory, child.name);
    const name = prefix ? `${prefix}/${child.name}` : child.name;
    if (child.isDirectory()) {
      entries.push(...(await collectFiles(absolute, name)));
      continue;
    }
    if (!child.isFile()) throw new Error(`Unsupported package entry: ${name}`);
    const info = await stat(absolute);
    entries.push({
      name,
      data: await readFile(absolute),
      mode: name.startsWith("bin/") ? 0o755 : (info.mode & 0o777) || 0o644,
    });
  }
  return entries;
}

function platformTarget() {
  const goos = { darwin: "darwin", linux: "linux", win32: "windows" }[process.platform];
  const goarch = { arm64: "arm64", x64: "x64" }[process.arch];
  if (!goos || !goarch) throw new Error(`Unsupported plugin target: ${process.platform}-${process.arch}`);
  return `${goos}-${goarch}`;
}

function goPlatform(value) {
  const [goos, arch] = value.split("-");
  const goarch = { arm64: "arm64", x64: "amd64" }[arch];
  if (!["darwin", "linux", "windows"].includes(goos) || !goarch) {
    throw new Error(`Unsupported DBX_PLUGIN_TARGET '${value}'; use darwin-arm64, darwin-x64, linux-x64, linux-arm64, or windows-x64`);
  }
  return [goos, goarch];
}

function sha256(buffer) {
  return createHash("sha256").update(buffer).digest("hex");
}

function crc32(buffer) {
  let crc = -1;
  for (const byte of buffer) crc = (crc >>> 8) ^ CRC_TABLE[(crc ^ byte) & 0xff];
  return (crc ^ -1) >>> 0;
}

/** Minimal ZIP writer: deflate, UTF-8 names, Unix modes, no directory entries. */
function writeZip(files) {
  const { time, date } = dosTimestamp(new Date());
  const local = [];
  const central = [];
  let offset = 0;
  for (const file of files) {
    const name = Buffer.from(file.name, "utf8");
    const compressed = deflateRawSync(file.data, { level: 9 });
    const crc = crc32(file.data);

    const header = Buffer.alloc(30);
    header.writeUInt32LE(0x04034b50, 0);
    header.writeUInt16LE(20, 4);
    header.writeUInt16LE(0x0800, 6);
    header.writeUInt16LE(8, 8);
    header.writeUInt16LE(time, 10);
    header.writeUInt16LE(date, 12);
    header.writeUInt32LE(crc, 14);
    header.writeUInt32LE(compressed.length, 18);
    header.writeUInt32LE(file.data.length, 22);
    header.writeUInt16LE(name.length, 26);
    header.writeUInt16LE(0, 28);
    local.push(header, name, compressed);

    const entry = Buffer.alloc(46);
    entry.writeUInt32LE(0x02014b50, 0);
    entry.writeUInt16LE(0x031e, 4);
    entry.writeUInt16LE(20, 6);
    entry.writeUInt16LE(0x0800, 8);
    entry.writeUInt16LE(8, 10);
    entry.writeUInt16LE(time, 12);
    entry.writeUInt16LE(date, 14);
    entry.writeUInt32LE(crc, 16);
    entry.writeUInt32LE(compressed.length, 20);
    entry.writeUInt32LE(file.data.length, 24);
    entry.writeUInt16LE(name.length, 28);
    entry.writeUInt16LE(0, 30);
    entry.writeUInt16LE(0, 32);
    entry.writeUInt16LE(0, 34);
    entry.writeUInt16LE(0, 36);
    entry.writeUInt32LE((((file.mode & 0o777) | 0o100000) << 16) >>> 0, 38);
    entry.writeUInt32LE(offset, 42);
    central.push(entry, name);

    offset += header.length + name.length + compressed.length;
  }
  const directory = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(0, 4);
  end.writeUInt16LE(0, 6);
  end.writeUInt16LE(files.length, 8);
  end.writeUInt16LE(files.length, 10);
  end.writeUInt32LE(directory.length, 12);
  end.writeUInt32LE(offset, 16);
  end.writeUInt16LE(0, 20);
  return Buffer.concat([...local, directory, end]);
}

function dosTimestamp(date) {
  const year = Math.max(1980, date.getFullYear());
  return {
    time: (date.getHours() << 11) | (date.getMinutes() << 5) | Math.floor(date.getSeconds() / 2),
    date: ((year - 1980) << 9) | ((date.getMonth() + 1) << 5) | date.getDate(),
  };
}

await main();
