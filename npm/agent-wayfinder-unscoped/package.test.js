const assert = require("node:assert/strict");
const test = require("node:test");

const packageInfo = require("./package.json");

test("unscoped package uses the matching canonical package version", () => {
	assert.equal(packageInfo.dependencies["@arcmantle/agent-wayfinder"], packageInfo.version);
});

test("unscoped package exposes all CLI command aliases", () => {
	const expectedBinary = "bin/agent-wayfinder.js";

	for (const command of ["agent-wayfinder", "a-wayfinder", "wayfind", "awf"]) {
		assert.equal(packageInfo.bin[command], expectedBinary);
	}
});