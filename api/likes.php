<?php
header('Content-Type: application/json');
header('Access-Control-Allow-Origin: https://prowl.sh');
header('Access-Control-Allow-Methods: GET, POST, OPTIONS');
header('Access-Control-Allow-Headers: Content-Type');

if ($_SERVER['REQUEST_METHOD'] === 'OPTIONS') {
    http_response_code(200);
    exit;
}

// Get visitor info for tracking
$ip = $_SERVER['HTTP_X_FORWARDED_FOR'] ?? $_SERVER['REMOTE_ADDR'] ?? 'unknown';
$user_agent = $_SERVER['HTTP_USER_AGENT'] ?? 'unknown';

// Function to get location data from IP (using free ipapi.co service)
function getLocationData($ip) {
    if ($ip === 'unknown' || $ip === '127.0.0.1' || strpos($ip, '192.168.') === 0) {
        return ['country_code' => null, 'country_name' => 'Local/Private', 'city' => 'Unknown'];
    }
    
    $url = "http://ipapi.co/{$ip}/json/";
    $context = stream_context_create([
        'http' => [
            'timeout' => 3,
            'method' => 'GET',
            'header' => 'User-Agent: ProwlLoveCounter/1.0'
        ]
    ]);
    
    $response = @file_get_contents($url, false, $context);
    if ($response) {
        $data = json_decode($response, true);
        if ($data && !isset($data['error'])) {
            return [
                'country_code' => $data['country_code'] ?? null,
                'country_name' => $data['country_name'] ?? 'Unknown',
                'city' => $data['city'] ?? 'Unknown'
            ];
        }
    }
    
    return ['country_code' => null, 'country_name' => 'Unknown', 'city' => 'Unknown'];
}

// Rate limiting (simple file-based)
$rate_file = __DIR__ . '/rate_' . md5($ip) . '.txt';
$now = time();

// Clean old rate limit files
if (file_exists($rate_file)) {
    $data = file_get_contents($rate_file);
    $parts = explode(':', $data);
    if (count($parts) === 2 && ($now - (int)$parts[1]) > 60) {
        unlink($rate_file);
    }
}

// Database connection
try {
    // Try multiple ways to get DATABASE_URL
    $database_url = getenv('DATABASE_URL') ?: $_ENV['DATABASE_URL'] ?: $_SERVER['DATABASE_URL'] ?: null;
    
    if (!$database_url) {
        http_response_code(503);
        echo json_encode(['error' => 'Service unavailable', 'count' => 0, 'debug' => 'DB_URL_MISSING']);
        exit;
    }
    
    // Handle special characters in DATABASE_URL by URL encoding the password part
    if (preg_match('/^postgresql:\/\/([^:]+):([^@]+)@([^:\/]+):?(\d+)?\/(.+)$/', $database_url, $matches)) {
        $user = $matches[1];
        $password = rawurldecode($matches[2]); // Decode URL-encoded password
        $host = $matches[3];
        $port = $matches[4] ?? 5432;
        $dbname = $matches[5];
        
        $dsn = "pgsql:host={$host};port={$port};dbname={$dbname}";
        $pdo = new PDO($dsn, $user, $password);
    } else {
        http_response_code(503);
        echo json_encode(['error' => 'Service unavailable', 'count' => 0, 'debug' => 'INVALID_DB_URL_FORMAT: ' . substr($database_url, 0, 50) . '...']);
        exit;
    }
    $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    
    // Create tables if not exists
    $pdo->exec("CREATE TABLE IF NOT EXISTS likes (
        id SERIAL PRIMARY KEY,
        count INTEGER DEFAULT 0,
        updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    )");
    
    $pdo->exec("CREATE TABLE IF NOT EXISTS like_origins (
        id SERIAL PRIMARY KEY,
        country_code VARCHAR(2),
        country_name VARCHAR(100),
        city VARCHAR(100),
        ip_hash VARCHAR(64),
        user_agent_hash VARCHAR(64),
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    )");
    
} catch (PDOException $e) {
    http_response_code(503);
    echo json_encode(['error' => 'Service unavailable', 'count' => 0, 'debug' => 'PDO_ERROR: ' . $e->getMessage()]);
    exit;
}

if ($_SERVER['REQUEST_METHOD'] === 'GET') {
    // Get current count
    try {
        $stmt = $pdo->prepare("SELECT count FROM likes WHERE id = 1");
        $stmt->execute();
        $result = $stmt->fetch(PDO::FETCH_ASSOC);
        
        if (!$result) {
            $pdo->prepare("INSERT INTO likes (id, count) VALUES (1, 0)")->execute();
            $count = 0;
        } else {
            $count = (int)$result['count'];
        }
        
        echo json_encode(['count' => $count]);
        
    } catch (PDOException $e) {
        http_response_code(503);
        echo json_encode(['error' => 'Service unavailable', 'count' => 0, 'debug' => 'GET_ERROR: ' . $e->getMessage()]);
    }
    
} else if ($_SERVER['REQUEST_METHOD'] === 'POST') {
    // Increment count
    
    // Check rate limit
    if (file_exists($rate_file)) {
        $data = file_get_contents($rate_file);
        $parts = explode(':', $data);
        if (count($parts) === 2 && (int)$parts[0] >= 10) {
            http_response_code(429);
            echo json_encode(['error' => 'Rate limited']);
            exit;
        }
    }
    
    try {
        $pdo->beginTransaction();
        
        $stmt = $pdo->prepare("SELECT count FROM likes WHERE id = 1 FOR UPDATE");
        $stmt->execute();
        $result = $stmt->fetch(PDO::FETCH_ASSOC);
        
        if (!$result) {
            $pdo->prepare("INSERT INTO likes (id, count) VALUES (1, 1)")->execute();
            $count = 1;
        } else {
            $current_count = (int)$result['count'];
            if ($current_count >= 999999999) {
                $pdo->rollBack();
                http_response_code(400);
                echo json_encode(['error' => 'Maximum likes reached', 'count' => $current_count]);
                exit;
            }
            
            $pdo->prepare("UPDATE likes SET count = count + 1, updated_at = CURRENT_TIMESTAMP WHERE id = 1")->execute();
            $stmt = $pdo->prepare("SELECT count FROM likes WHERE id = 1");
            $stmt->execute();
            $result = $stmt->fetch(PDO::FETCH_ASSOC);
            $count = (int)$result['count'];
        }
        
        // Track like origin (async, don't block on failures)
        try {
            $location = getLocationData($ip);
            $ip_hash = hash('sha256', $ip . 'prowl_salt_2024'); // Hash IP for privacy
            $ua_hash = hash('sha256', $user_agent . 'prowl_salt_2024'); // Hash user agent
            
            $stmt = $pdo->prepare("INSERT INTO like_origins (country_code, country_name, city, ip_hash, user_agent_hash) VALUES (?, ?, ?, ?, ?)");
            $stmt->execute([
                $location['country_code'],
                $location['country_name'],
                $location['city'],
                $ip_hash,
                $ua_hash
            ]);
        } catch (Exception $e) {
            // Don't fail the like if tracking fails
            error_log("Like tracking failed: " . $e->getMessage());
        }
        
        $pdo->commit();
        
        // Update rate limit (with error handling)
        $current_count = 1;
        if (file_exists($rate_file)) {
            $data = file_get_contents($rate_file);
            $parts = explode(':', $data);
            if (count($parts) === 2) {
                $current_count = (int)$parts[0] + 1;
            }
        }
        @file_put_contents($rate_file, $current_count . ':' . $now); // Suppress warnings
        
        echo json_encode(['count' => $count]);
        
    } catch (PDOException $e) {
        $pdo->rollBack();
        http_response_code(503);
        echo json_encode(['error' => 'Service unavailable', 'debug' => 'POST_ERROR: ' . $e->getMessage()]);
    }
    
} else {
    http_response_code(405);
    echo json_encode(['error' => 'Method not allowed']);
}
?>