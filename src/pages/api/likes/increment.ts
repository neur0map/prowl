import type { APIRoute } from 'astro';
import { Client } from 'pg';

// Rate limiting storage (in production, use Redis or similar)
const rateLimits = new Map<string, { count: number, resetTime: number }>();
const RATE_LIMIT_WINDOW = 60000; // 1 minute
const MAX_INCREMENTS_PER_WINDOW = 10; // More restrictive for increments

function getRealIP(request: Request): string {
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
  
  if (limit.count >= MAX_INCREMENTS_PER_WINDOW) {
    return true;
  }
  
  limit.count++;
  return false;
}

export const POST: APIRoute = async ({ request }) => {
  // Environment validation
  if (!process.env.DATABASE_URL) {
    return new Response(JSON.stringify({ error: 'Service unavailable' }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' }
    });
  }

  // Rate limiting
  const clientIP = getRealIP(request);
  if (isRateLimited(clientIP)) {
    return new Response(JSON.stringify({ error: 'Rate limited' }), {
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
    return new Response(JSON.stringify({ error: 'Unauthorized' }), {
      status: 403,
      headers: { 'Content-Type': 'application/json' }
    });
  }

  // Validate request method
  if (request.method !== 'POST') {
    return new Response(JSON.stringify({ error: 'Method not allowed' }), {
      status: 405,
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

    // Use transaction for consistency
    await client.query('BEGIN');
    
    let result = await client.query('SELECT count FROM likes WHERE id = 1');
    
    if (result.rows.length === 0) {
      await client.query('INSERT INTO likes (id, count) VALUES (1, 1)');
      result = await client.query('SELECT count FROM likes WHERE id = 1');
    } else {
      // Add bounds checking to prevent overflow
      const currentCount = result.rows[0].count;
      if (currentCount >= 999999999) { // Reasonable upper limit
        await client.query('ROLLBACK');
        return new Response(JSON.stringify({ error: 'Maximum likes reached', count: currentCount }), {
          status: 400,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      
      await client.query('UPDATE likes SET count = count + 1, updated_at = CURRENT_TIMESTAMP WHERE id = 1');
      result = await client.query('SELECT count FROM likes WHERE id = 1');
    }

    await client.query('COMMIT');
    const count = result.rows[0].count;

    return new Response(JSON.stringify({ count }), {
      status: 200,
      headers: {
        'Content-Type': 'application/json',
        'Cache-Control': 'no-cache, no-store, must-revalidate',
      },
    });

  } catch (error) {
    if (client) {
      try {
        await client.query('ROLLBACK');
      } catch (e) {
        // Ignore rollback errors
      }
    }
    // No logging to prevent any info leakage
    return new Response(JSON.stringify({ error: 'Service unavailable' }), {
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

export const OPTIONS: APIRoute = async ({ request }) => {
  // Validate origin for CORS preflight
  const origin = request.headers.get('origin');
  const allowedDomains = ['prowl.sh', 'www.prowl.sh', 'localhost', '127.0.0.1'];
  const isDevelopment = process.env.NODE_ENV !== 'production';
  
  // In development, be more permissive; in production, strict origin checking
  if (!isDevelopment && origin && !allowedDomains.some(domain => origin.includes(domain))) {
    return new Response(null, {
      status: 403,
      headers: { 'Content-Type': 'application/json' }
    });
  }

  return new Response(null, {
    status: 200,
    headers: {
      'Access-Control-Allow-Methods': 'POST, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type',
      'Access-Control-Max-Age': '86400', // Cache preflight for 24 hours
    },
  });
};