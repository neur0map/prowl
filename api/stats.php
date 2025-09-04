<?php
header('Content-Type: application/json');
header('Access-Control-Allow-Origin: https://prowl.sh');
header('Access-Control-Allow-Methods: GET, OPTIONS');
header('Access-Control-Allow-Headers: Content-Type');

if ($_SERVER['REQUEST_METHOD'] === 'OPTIONS') {
    http_response_code(200);
    exit;
}

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    http_response_code(405);
    echo json_encode(['error' => 'Method not allowed']);
    exit;
}

// Database connection
try {
    $database_url = getenv('DATABASE_URL') ?: $_ENV['DATABASE_URL'] ?: $_SERVER['DATABASE_URL'] ?: null;
    
    if (!$database_url) {
        http_response_code(503);
        echo json_encode(['error' => 'Service unavailable']);
        exit;
    }
    
    if (preg_match('/^postgresql:\/\/([^:]+):([^@]+)@([^:\/]+):?(\d+)?\/(.+)$/', $database_url, $matches)) {
        $user = $matches[1];
        $password = rawurldecode($matches[2]);
        $host = $matches[3];
        $port = $matches[4] ?? 5432;
        $dbname = $matches[5];
        
        $dsn = "pgsql:host={$host};port={$port};dbname={$dbname}";
        $pdo = new PDO($dsn, $user, $password);
    } else {
        http_response_code(503);
        echo json_encode(['error' => 'Service unavailable']);
        exit;
    }
    
    $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    
    // Get total likes
    $stmt = $pdo->prepare("SELECT count FROM likes WHERE id = 1");
    $stmt->execute();
    $result = $stmt->fetch(PDO::FETCH_ASSOC);
    $total_likes = $result ? (int)$result['count'] : 0;
    
    // Get country stats
    $stmt = $pdo->prepare("
        SELECT 
            country_name,
            country_code,
            COUNT(*) as like_count
        FROM like_origins 
        WHERE country_name IS NOT NULL AND country_name != 'Unknown'
        GROUP BY country_name, country_code
        ORDER BY like_count DESC
        LIMIT 10
    ");
    $stmt->execute();
    $country_stats = $stmt->fetchAll(PDO::FETCH_ASSOC);
    
    // Get city stats
    $stmt = $pdo->prepare("
        SELECT 
            city,
            country_name,
            COUNT(*) as like_count
        FROM like_origins 
        WHERE city IS NOT NULL AND city != 'Unknown'
        GROUP BY city, country_name
        ORDER BY like_count DESC
        LIMIT 10
    ");
    $stmt->execute();
    $city_stats = $stmt->fetchAll(PDO::FETCH_ASSOC);
    
    // Get recent activity (last 24 hours)
    $stmt = $pdo->prepare("
        SELECT COUNT(*) as recent_likes
        FROM like_origins 
        WHERE created_at >= NOW() - INTERVAL '24 hours'
    ");
    $stmt->execute();
    $recent_result = $stmt->fetch(PDO::FETCH_ASSOC);
    $recent_likes = $recent_result ? (int)$recent_result['recent_likes'] : 0;
    
    echo json_encode([
        'total_likes' => $total_likes,
        'recent_likes_24h' => $recent_likes,
        'top_countries' => $country_stats,
        'top_cities' => $city_stats,
        'generated_at' => date('Y-m-d H:i:s') . ' UTC'
    ], JSON_PRETTY_PRINT);
    
} catch (PDOException $e) {
    http_response_code(503);
    echo json_encode(['error' => 'Service unavailable']);
}
?>