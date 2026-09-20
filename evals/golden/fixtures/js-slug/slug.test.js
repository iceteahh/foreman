'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { slugify } = require('./slug');

test('lowercases and joins words', () => {
  assert.strictEqual(slugify('Hello Wide World'), 'hello-wide-world');
});

test('trims separators', () => {
  assert.strictEqual(slugify('  --Hello--  '), 'hello');
});
