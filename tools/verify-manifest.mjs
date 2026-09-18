#!/usr/bin/env node
// Validates manifest.json and the packaged layout without needing DBX.
// Used by CI; also handy before packaging locally.

import { readFileSync, existsSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const problems = [];

function fail(message) {
  problems.push(message);
}

function readJson(relativePath) {
  const absolute = join(projectRoot, relativePath);
  if (!existsSync(absolute)) {
    fail(`missing file: ${relativePath}`);
    return null;
  }
  try {
    return JSON.parse(readFileSync(absolute, "utf8"));
  } catch (error) {
    fail(`${relativePath} is not valid JSON: ${error.message}`);
    return null;
  }
}

const manifest = readJson("manifest.json");
const toml = existsSync(join(projectRoot, "dbx-plugin.toml"))
  ? readFileSync(join(projectRoot, "dbx-plugin.toml"), "utf8")
  : null;

if (!toml) {
  fail("missing file: dbx-plugin.toml");
}

if (manifest) {
  if (manifest.manifest_version !== 1) {
    fail(`manifest_version must be 1, found ${JSON.stringify(manifest.manifest_version)}`);
  }

  for (const field of ["id", "name", "version", "publisher"]) {
    if (typeof manifest[field] !== "string" || manifest[field].length === 0) {
      fail(`manifest.${field} must be a non-empty string`);
    }
  }

  const id = String(manifest.id ?? "");
  if (id && !/^[a-z0-9._-]+$/.test(id)) {
    fail(`manifest.id may only contain lowercase letters, digits, '.', '_' and '-': ${id}`);
  }

  const version = String(manifest.version ?? "");
  if (version && !/^\d+\.\d+\.\d+/.test(version)) {
    fail(`manifest.version must be semantic (major.minor.patch): ${version}`);
  }

  if (!manifest.engines || typeof manifest.engines.host_api !== "string") {
    fail("manifest.engines.host_api is required");
  }

  const allowedPermissions = ["host.events", "host.binary", "host.workbench", "host.filesystem"];
  for (const permission of manifest.permissions ?? []) {
    if (!allowedPermissions.includes(permission) && !permission.startsWith("host.network:")) {
      fail(`unknown permission: ${permission}`);
    }
  }

  const allowedContributions = [
    "connection-provider",
    "workbench",
    "filesystem-provider",
    "context-menu",
    "result-view",
  ];
  const contributionIds = new Set();
  for (const contribution of manifest.contributions ?? []) {
    if (!allowedContributions.includes(contribution.type)) {
      fail(`unknown contribution type: ${contribution.type}`);
    }
    if (contributionIds.has(contribution.id)) {
      fail(`duplicate contribution id: ${contribution.id}`);
    }
    contributionIds.add(contribution.id);
  }

  for (const contribution of manifest.contributions ?? []) {
    if (contribution.type === "connection-provider") {
      if (!contribution.database_type) {
        fail(`connection-provider ${contribution.id} needs database_type`);
      }
      if (contribution.workbench && !contributionIds.has(contribution.workbench)) {
        fail(`connection-provider ${contribution.id} references unknown workbench ${contribution.workbench}`);
      }
      for (const field of contribution.fields ?? []) {
        if (!field.key || !field.label || !field.type) {
          fail(`connection-provider ${contribution.id}: each field needs key, label and type`);
        }
        if ((field.type === "select" || field.type === "radio") && !(field.options ?? []).length) {
          fail(`connection-provider ${contribution.id}: field ${field.key} needs options`);
        }
      }
    }
  }

  // Every referenced asset must exist inside the package.
  const assets = [manifest.icon];
  for (const contribution of manifest.contributions ?? []) {
    if (contribution.icon) {
      assets.push(contribution.icon);
    }
  }
  for (const asset of assets) {
    if (!asset) {
      continue;
    }
    if (asset.startsWith("/") || asset.includes("..") || asset.includes("\\")) {
      fail(`asset path must be a plain relative path: ${asset}`);
      continue;
    }
    if (!existsSync(join(projectRoot, asset))) {
      fail(`referenced asset is missing: ${asset}`);
    }
  }

  const ui = manifest.entrypoints?.ui;
  if (!ui || !ui.root || !ui.entry) {
    fail("manifest.entrypoints.ui needs root and entry");
  } else {
    if (!ui.entry.startsWith(ui.root + "/")) {
      fail(`ui.entry must live inside ui.root (${ui.root}), found ${ui.entry}`);
    }
    if (!existsSync(join(projectRoot, ui.entry))) {
      fail(`ui.entry does not exist: ${ui.entry}`);
    }
  }

  // Directories that dbx-plugin.toml promises to package must exist.
  const includeMatch = toml?.match(/include\s*=\s*\[([^\]]*)\]/);
  if (includeMatch) {
    for (const raw of includeMatch[1].split(",")) {
      const entry = raw.trim().replace(/^"|"$/g, "");
      if (entry && !existsSync(join(projectRoot, entry))) {
        fail(`dbx-plugin.toml includes a directory that does not exist: ${entry}`);
      }
    }
  }
}

if (problems.length) {
  console.error("manifest verification failed:");
  for (const problem of problems) {
    console.error("  - " + problem);
  }
  process.exit(1);
}

console.log("manifest verification passed");
console.log(`  id      : ${manifest.id}`);
console.log(`  version : ${manifest.version}`);
console.log(`  contributions: ${(manifest.contributions ?? []).map((item) => item.type).join(", ")}`);