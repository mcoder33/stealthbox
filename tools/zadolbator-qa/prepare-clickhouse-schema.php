<?php

declare(strict_types=1);

// Extract inert SQL from the selected checkout; never execute migration PHP.
function failSchemaPreparation(string $message): void
{
    fwrite(STDERR, 'ERROR: QA ClickHouse schema: ' . $message . PHP_EOL);
    exit(1);
}

if ($argc !== 2 || !is_file($argv[1])) {
    failSchemaPreparation('expected one readable canonical migration path');
}
$source = file_get_contents($argv[1]);
if ($source === false) {
    failSchemaPreparation('cannot read canonical migration');
}
// This migration uses one non-interpolated literal. A different shape needs review.
$literalPattern = '/\$client->write\(\s*"([^"\\\\$]*)"\s*\);/s';
if (preg_match_all($literalPattern, $source, $literals) !== 1
    || preg_match_all('/->write\s*\(/', $source) !== 1) {
    failSchemaPreparation('expected exactly one non-interpolated SQL literal');
}
$ddlPattern = '/\A\s*create\s+table\s+if\s+not\s+exists\s+`zb-message_queue_wide-cmt`\s*\((.*)\)\s*engine\s*=\s*CollapsingMergeTree\(sign\)\s*ORDER\s+BY\s+id\s*;?\s*\z/is';
if (preg_match($ddlPattern, $literals[1][0], $ddl) !== 1) {
    failSchemaPreparation('unexpected table, engine, order or extra SQL');
}
$scalarType = '(?:UInt(?:8|16|32|64)|Int(?:8|16|32|64)|Float(?:32|64)|String|Date|DateTime)';
$columnPattern = '/\A\s*([A-Za-z_][A-Za-z0-9_]*)\s+(' . $scalarType . '|Nullable\(' . $scalarType . '\))\s*\z/';
$columnTypes = [];
foreach (explode(',', $ddl[1]) as $definition) {
    if (preg_match($columnPattern, $definition, $column) !== 1 || isset($columnTypes[$column[1]])) {
        failSchemaPreparation('unsupported or duplicate column definition');
    }
    $columnTypes[$column[1]] = $column[2];
}
foreach (['id' => 'UInt64', 'sign' => 'Int8', 'status' => 'UInt8', 'attributionDate' => 'Nullable(DateTime)'] as $name => $type) {
    if (($columnTypes[$name] ?? null) !== $type) {
        failSchemaPreparation('required date-grid column/type is missing');
    }
}
// Fixed framing confines the extracted columns to this empty, local QA table.
echo "CREATE TABLE IF NOT EXISTS `qa`.`zb-message_queue_wide-cmt`\n(";
echo $ddl[1];
echo ")\nENGINE = CollapsingMergeTree(sign)\nORDER BY id;\n";
