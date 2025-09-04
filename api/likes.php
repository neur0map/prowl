<?php
header('Content-Type: application/json');
header('Access-Control-Allow-Origin: https://prowl.sh');
header('Access-Control-Allow-Methods: GET, POST, OPTIONS');
header('Access-Control-Allow-Headers: Content-Type');

if ($_SERVER['REQUEST_METHOD'] === 'OPTIONS') {
    http_response_code(200);
    exit;
}

// Rate limiting (simple file-based)
$ip = $_SERVER['HTTP_X_FORWARDED_FOR'] ?? $_SERVER['REMOTE_ADDR'] ?? 'unknown';
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
    
    $pdo = new PDO($database_url);
    $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    
    // Create table if not exists
    $pdo->exec("CREATE TABLE IF NOT EXISTS likes (
        id SERIAL PRIMARY KEY,
        count INTEGER DEFAULT 0,
        updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    )");
    
} catch (PDOException $e) {
    http_response_code(503);
    echo json_encode(['error' => 'Service unavailable', 'count' => 0]);
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
        echo json_encode(['error' => 'Service unavailable', 'count' => 0]);
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
        
        $pdo->commit();
        
        // Update rate limit
        $current_count = 1;
        if (file_exists($rate_file)) {
            $data = file_get_contents($rate_file);
            $parts = explode(':', $data);
            if (count($parts) === 2) {
                $current_count = (int)$parts[0] + 1;
            }
        }
        file_put_contents($rate_file, $current_count . ':' . $now);
        
        echo json_encode(['count' => $count]);
        
    } catch (PDOException $e) {
        $pdo->rollBack();
        http_response_code(503);
        echo json_encode(['error' => 'Service unavailable']);
    }
    
} else {
    http_response_code(405);
    echo json_encode(['error' => 'Method not allowed']);
}
?>