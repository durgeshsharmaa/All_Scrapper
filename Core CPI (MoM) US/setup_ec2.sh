#!/bin/bash
set -e

echo "=== Step 1: Installing Go 1.23.5 ==="
curl -sL https://go.dev/dl/go1.23.5.linux-amd64.tar.gz -o /tmp/go.tar.gz
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc

echo "=== Go version check ==="
/usr/local/go/bin/go version

echo "=== Step 2: Build binary ==="
cd /home/ec2-user/cpi_sniper
/usr/local/go/bin/go mod tidy
/usr/local/go/bin/go build -o cpi_sniper main.go
echo "Binary built: $(ls -lh cpi_sniper)"

echo "=== Step 3: Create systemd service ==="
sudo tee /etc/systemd/system/cpi_sniper.service > /dev/null <<'EOF'
[Unit]
Description=Core CPI (MoM) US Sniper Scraper
After=network.target

[Service]
Type=simple
User=ec2-user
WorkingDirectory=/home/ec2-user/cpi_sniper
ExecStart=/home/ec2-user/cpi_sniper/cpi_sniper
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

echo "=== Step 4: Enable & Start service ==="
sudo systemctl daemon-reload
sudo systemctl enable cpi_sniper
sudo systemctl start cpi_sniper

sleep 2
echo "=== Service Status ==="
sudo systemctl status cpi_sniper --no-pager

echo "=== DEPLOYMENT COMPLETE ==="
