'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
test('shipped config does not enable HA or old OTA', () => {
  const config = fs.readFileSync(path.join(__dirname, '../voice/manifest/config/config.yaml'), 'utf8');
  for (const pattern of [
    /^  local_host: "127\.0\.0\.1"$/m,
    /^  ha_enabled: false$/m,
    /^  standalone_ha_enabled: false$/m,
    /^  ota_https_enabled: false$/m,
    /^  upstream_ota_url: ""$/m,
    /^  ha_mcp_url: ""$/m
  ]) assert.match(config, pattern);
  assert.doesNotMatch(config, /(?:api\.)?tenclass\.net|xiaozhi\.me/i);
});
