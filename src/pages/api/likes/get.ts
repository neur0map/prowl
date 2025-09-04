import type { APIRoute } from 'astro';
import { Client } from 'pg';

// Rate limiting storage (in production, use Redis or similar)
const rateLimits = new Map<string, { count: number, resetTime: number }>();
const RATE_LIMIT_WINDOW = 60000; // 1 minute
const MAX_REQUESTS_PER_WINDOW = 30;

function getRealIP(request: Request): string {
  // Get real IP from headers (for production behind proxy)
  const forwarded = request.headers.get('x-forwarded-for');
  const realIP = request.headers.get('x-real-ip');
  return forwarded?.split(',')[0] || realIP || 'unknown';
}

function isRateLimited(ip: string): boolean {
  const now = Date.now();
  const limit = rateLimits.get(ip);
  
  if (!limit || now > limit.resetTime) {
    rateLimits.set(ip, { count: 1, resetTime: now + RATE_LIMIT_WINDOW });
    return false;
  }
  
  if (limit.count >= MAX_REQUESTS_PER_WINDOW) {
    return true;
  }
  
  limit.count++;
  return false;
}

export const GET: APIRoute = async ({ request }) => {
  // Environment validation
  if (!process.env.DATABASE_URL) {
    console.error('DATABASE_URL environment variable not set');
    return new Response(JSON.stringify({ error: 'Server configuration error', count: 0 }), {
      status: 500,
      headers: { 'Content-Type': 'application/json' }
    });
  }

  // Rate limiting
  const clientIP = getRealIP(request);
  if (isRateLimited(clientIP)) {
    return new Response(JSON.stringify({ error: 'Rate limited', count: 0 }), {
      status: 429,
      headers: { 
        'Content-Type': 'application/json',
        'Retry-After': '60'
      }
    });
  }

  // Validate origin for additional security
  const origin = request.headers.get('origin');
  const referer = request.headers.get('referer');
  const allowedDomains = ['prowl.sh', 'www.prowl.sh', 'localhost', '127.0.0.1'];
  const isDevelopment = process.env.NODE_ENV !== 'production';
  
  // In development, be more permissive; in production, strict origin checking
  if (!isDevelopment && origin && !allowedDomains.some(domain => origin.includes(domain))) {
    return new Response(JSON.stringify({ error: 'Unauthorized', count: 0 }), {
      status: 403,
      headers: { 'Content-Type': 'application/json' }
    });
  }

  let client: Client | null = null;
  
  try {
    client = new Client({
      connectionString: process.env.DATABASE_URL,
      connectionTimeoutMillis: 5000,
      query_timeout: 10000,
      ssl: process.env.NODE_ENV === 'production' ? { rejectUnauthorized: false } : false
    });
    
    await client.connect();
    
    // Create table if not exists
    await client.query(`
      CREATE TABLE IF NOT EXISTS likes (
        id SERIAL PRIMARY KEY,
        count INTEGER DEFAULT 0,
        updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
      )
    `);

    let result = await client.query('SELECT count FROM likes WHERE id = 1');
    
    if (result.rows.length === 0) {
      await client.query('INSERT INTO likes (id, count) VALUES (1, 0)');
      result = await client.query('SELECT count FROM likes WHERE id = 1');
    }

    const count = result.rows[0].count;

    return new Response(JSON.stringify({ count }), {
      status: 200,
      headers: {
        'Content-Type': 'application/json',
        'Cache-Control': 'no-cache, no-store, must-revalidate',
      },
    });

  } catch (error) {
    // Don't log full error details to prevent info leakage
    console.error('Database operation failed');
    return new Response(JSON.stringify({ error: 'Service unavailable', count: 0 }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' }
    });
  } finally {
    if (client) {
      try {
        await client.end();
      } catch (e) {
        // Ignore cleanup errors
      }
    }
  }
};