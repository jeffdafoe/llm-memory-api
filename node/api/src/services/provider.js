// Provider abstraction — thin re-export from providers/ directory.
// All provider logic, model registries, and capabilities now live in providers/*.js.
// This file exists for backward compatibility with existing require() paths.

module.exports = require('./providers');
