#!/bin/bash
set -e

echo "Downloading Go..."
wget -q https://go.dev/dl/go1.23.5.linux-amd64.tar.gz
echo "Installing Go..."
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.23.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
go version

echo "Building scraper..."
go mod tidy
go build -o gdp_scrapper main.go

echo "Setting up Systemd service..."
cat << 'EOF' | sudo tee /etc/systemd/system/gdp_scrapper.service
[Unit]
Description=Canada GDP Scraper Service
After=network.target

[Service]
User=ec2-user
Group=ec2-user
WorkingDirectory=/home/ec2-user
ExecStart=/home/ec2-user/gdp_scrapper
Restart=always
RestartSec=60

[Install]
WantedBy=multi-user.target
EOF

echo "Starting service..."
sudo systemctl daemon-reload
sudo systemctl enable gdp_scrapper
sudo systemctl restart gdp_scrapper
sudo systemctl status gdp_scrapper --no-pager
echo "Done!"
