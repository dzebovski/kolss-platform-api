/**
 * KOLSS CRM — incremental Meta Lead Ads import.
 *
 * Install the same script in the Kyiv and Warsaw spreadsheets with different
 * IMPORT_WEBHOOK_SECRET and SOURCE_ID values.
 *
 * Script properties:
 *   CRM_WEBHOOK_URL=https://api.kolss.eu/v1/integrations/google-sheets/lead-imports
 *   IMPORT_WEBHOOK_SECRET=<office-specific secret>
 *   SOURCE_ID=<lead_import_sources uuid>
 *   SHEET_NAME=<tab name>
 *   HEADER_ROW=1
 *   BATCH_SIZE=20
 *   LAST_ROW=<keep the existing value during cutover>
 */

var PROP = {
  WEBHOOK_URL: 'CRM_WEBHOOK_URL',
  SECRET: 'IMPORT_WEBHOOK_SECRET',
  SOURCE_ID: 'SOURCE_ID',
  SHEET_NAME: 'SHEET_NAME',
  HEADER_ROW: 'HEADER_ROW',
  LAST_ROW: 'LAST_ROW',
  BATCH_SIZE: 'BATCH_SIZE',
};

function getConfig_() {
  var props = PropertiesService.getScriptProperties();
  var config = {
    webhookUrl: props.getProperty(PROP.WEBHOOK_URL),
    secret: props.getProperty(PROP.SECRET),
    sourceId: props.getProperty(PROP.SOURCE_ID),
    sheetName: props.getProperty(PROP.SHEET_NAME) || 'Sheet1',
    headerRow: parseInt(props.getProperty(PROP.HEADER_ROW) || '1', 10),
    batchSize: Math.min(100, parseInt(props.getProperty(PROP.BATCH_SIZE) || '20', 10)),
  };
  if (!config.webhookUrl || !config.secret || !config.sourceId) {
    throw new Error('Missing CRM_WEBHOOK_URL, IMPORT_WEBHOOK_SECRET, or SOURCE_ID');
  }
  return config;
}

function getSheet_(name) {
  var sheet = SpreadsheetApp.getActiveSpreadsheet().getSheetByName(name);
  if (!sheet) throw new Error('Sheet not found: ' + name);
  return sheet;
}

function collectRows_(sheet, headerRow, startRow, endRow) {
  if (endRow < startRow || sheet.getLastColumn() < 1) return [];
  var lastCol = sheet.getLastColumn();
  var headers = sheet.getRange(headerRow, 1, 1, lastCol).getValues()[0].map(function (value) {
    return String(value || '').trim();
  });
  var values = sheet.getRange(startRow, 1, endRow - startRow + 1, lastCol).getValues();
  var records = [];
  for (var i = 0; i < values.length; i++) {
    var row = values[i];
    var record = { _sheet_row: startRow + i };
    var hasValue = false;
    for (var column = 0; column < headers.length; column++) {
      if (!headers[column]) continue;
      var value = row[column] == null ? '' : String(row[column]).trim();
      if (value) hasValue = true;
      record[headers[column]] = value;
    }
    if (hasValue) records.push(record);
  }
  return records;
}

function postBatch_(config, mode, rows) {
  var response = UrlFetchApp.fetch(config.webhookUrl, {
    method: 'post',
    contentType: 'application/json',
    headers: { Authorization: 'Bearer ' + config.secret },
    payload: JSON.stringify({ source_id: config.sourceId, mode: mode, rows: rows }),
    muteHttpExceptions: true,
  });
  var code = response.getResponseCode();
  if (code < 200 || code >= 300) {
    throw new Error('CRM import failed (' + code + '): ' + response.getContentText());
  }
  return JSON.parse(response.getContentText());
}

function sendInBatches_(config, mode, records) {
  var summary = { processed: 0, created: 0, updated: 0, skipped: 0 };
  for (var i = 0; i < records.length; i += config.batchSize) {
    var result = postBatch_(config, mode, records.slice(i, i + config.batchSize));
    summary.processed += result.processed || 0;
    summary.created += result.created || 0;
    summary.updated += result.updated || 0;
    summary.skipped += result.skipped || 0;
  }
  return summary;
}

/** Incremental sync. Missing LAST_ROW initializes at the current last row. */
function syncNewLeads() {
  var config = getConfig_();
  var props = PropertiesService.getScriptProperties();
  var sheet = getSheet_(config.sheetName);
  var sheetLastRow = sheet.getLastRow();
  var lastRowRaw = props.getProperty(PROP.LAST_ROW);
  if (lastRowRaw === null || lastRowRaw === '') {
    props.setProperty(PROP.LAST_ROW, String(sheetLastRow));
    Logger.log('LAST_ROW initialized to ' + sheetLastRow + '; historical rows were not sent');
    return;
  }
  var startRow = Math.max(config.headerRow + 1, parseInt(lastRowRaw, 10) + 1);
  if (startRow > sheetLastRow) return;
  var records = collectRows_(sheet, config.headerRow, startRow, sheetLastRow);
  var summary = sendInBatches_(config, 'incremental', records);
  props.setProperty(PROP.LAST_ROW, String(sheetLastRow));
  Logger.log(JSON.stringify(summary));
}

/** Pre-cutover check: last 20 rows, deduplicated, and never enqueues Telegram. */
function reconcileLast20() {
  var config = getConfig_();
  var sheet = getSheet_(config.sheetName);
  var endRow = sheet.getLastRow();
  var startRow = Math.max(config.headerRow + 1, endRow - 19);
  var summary = sendInBatches_(config, 'reconcile', collectRows_(sheet, config.headerRow, startRow, endRow));
  Logger.log(JSON.stringify(summary));
}

function installTrigger() {
  ScriptApp.getProjectTriggers().forEach(function (trigger) {
    if (trigger.getHandlerFunction() === 'syncNewLeads') ScriptApp.deleteTrigger(trigger);
  });
  ScriptApp.newTrigger('syncNewLeads').timeBased().everyMinutes(5).create();
}
