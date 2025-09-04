# Build Astro site
FROM node:18 as builder
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build

# Serve with Apache + PHP
FROM php:8.2-apache
# Copy built static files
COPY --from=builder /app/dist/ /var/www/html/
# Copy PHP API
COPY api/ /var/www/html/api/

# Install PostgreSQL extension
RUN apt-get update && apt-get install -y libpq-dev
RUN docker-php-ext-install pdo pdo_pgsql

# Enable Apache modules
RUN a2enmod rewrite

# Configure Apache for SPA
RUN echo '<Directory "/var/www/html">\n\
    Options Indexes FollowSymLinks\n\
    AllowOverride All\n\
    Require all granted\n\
    RewriteEngine On\n\
    RewriteCond %{REQUEST_FILENAME} !-f\n\
    RewriteCond %{REQUEST_FILENAME} !-d\n\
    RewriteRule . /index.html [L]\n\
</Directory>' > /etc/apache2/conf-available/spa.conf

RUN a2enconf spa