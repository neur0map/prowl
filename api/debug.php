<?php
header('Content-Type: application/json');
header('Access-Control-Allow-Origin: https://prowl.sh');

// Debug environment variables
$debug_info = [
    'DATABASE_URL' => getenv('DATABASE_URL') ? 'SET' : 'NOT_SET',
    'NODE_ENV' => getenv('NODE_ENV'),
    'PORT' => getenv('PORT'),
    'HOST' => getenv('HOST'),
    'env_vars' => array_keys($_ENV),
    'server_vars' => isset($_SERVER['DATABASE_URL']) ? 'SERVER_SET' : 'SERVER_NOT_SET'
];

echo json_encode($debug_info, JSON_PRETTY_PRINT);
?>