#!/usr/bin/env bash
set -euo pipefail

# Configuration
GITHUB_REPO="oszuidwest/zwfm-encoder"
ENCODER_SERVICE_URL="https://raw.githubusercontent.com/${GITHUB_REPO}/main/deploy/encoder.service"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/encoder"
SERVICE_PATH="/etc/systemd/system/encoder.service"

# Functions library
FUNCTIONS_LIB_PATH=$(mktemp)
FUNCTIONS_LIB_URL="https://raw.githubusercontent.com/oszuidwest/bash-functions/main/common-functions.sh"

# Clean up temporary file on exit
trap 'rm -f "$FUNCTIONS_LIB_PATH"' EXIT

# General Raspberry Pi configuration
CONFIG_FILE_PATHS=("/boot/firmware/config.txt" "/boot/config.txt")
FIRST_IP=$(hostname -I | awk '{print $1}')

# Start with a clean terminal
clear

# Download the functions library
if ! curl -s -o "$FUNCTIONS_LIB_PATH" "$FUNCTIONS_LIB_URL"; then
  echo -e "*** Failed to download functions library. Please check your network connection! ***"
  exit 1
fi

# Source the functions file
# shellcheck source=/dev/null
source "$FUNCTIONS_LIB_PATH"

# Set color variables and perform initial checks
set_colors
check_user_privileges privileged
is_this_linux
is_this_os_64bit
check_rpi_model 4

# Determine the correct config file path
CONFIG_FILE=""
for path in "${CONFIG_FILE_PATHS[@]}"; do
  if [ -f "$path" ]; then
    CONFIG_FILE="$path"
    break
  fi
done

if [ -z "$CONFIG_FILE" ]; then
  echo -e "${RED}Error: config.txt not found in known locations.${NC}"
  exit 1
fi

# Check if the required tools are installed
require_tool curl systemctl

# Banner
cat << "EOF"
 ______     _     ___          __       _     ______ __  __
|___  /    (_)   | \ \        / /      | |   |  ____|  \/  |
   / /_   _ _  __| |\ \  /\  / /__  ___| |_  | |__  | \  / |
  / /| | | | |/ _` | \ \/  \/ / _ \/ __| __| |  __| | |\/| |
 / /_| |_| | | (_| |  \  /\  /  __/\__ \ |_  | |    | |  | |
/_____\__,_|_|\__,_|   \/  \/ \___||___/\__| |_|    |_|  |_|
EOF

# Greeting
echo -e "${GREEN}⎎ Audio encoder set-up for Raspberry Pi${NC}\n"

# Check if the HiFiBerry is configured
if ! grep -q "^dtoverlay=hifiberry" "$CONFIG_FILE"; then
  echo -e "${RED}No HiFiBerry card configured in the $CONFIG_FILE file. Exiting...${NC}\n" >&2
  exit 1
fi

# Ask for OS updates
ask_user "DO_UPDATES" "y" "Do you want to perform all OS updates? (y/n)" "y/n"

# Ask for heartbeat monitoring (read from /dev/tty to work with bash -c)
ENABLE_HEARTBEAT="n"
while true; do
  read -r -p "Do you want to enable heartbeat monitoring via UptimeRobot? (y/n) [default: n]: " answer < /dev/tty
  answer="${answer:-n}"
  if [[ "$answer" =~ ^[yn]$ ]]; then
    ENABLE_HEARTBEAT="$answer"
    break
  fi
  echo "Invalid input. Please enter 'y' or 'n'."
done

if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  while true; do
    read -r -p "Enter the heartbeat URL to ping every minute: " HEARTBEAT_URL < /dev/tty
    if [[ -n "$HEARTBEAT_URL" ]]; then
      break
    fi
    echo "URL cannot be empty."
  done
fi

# Ask for beta version installation
INSTALL_BETA="n"
while true; do
  read -r -p "Do you want to install a beta/prerelease version? (y/n) [default: n]: " answer < /dev/tty
  answer="${answer:-n}"
  if [[ "$answer" =~ ^[yn]$ ]]; then
    INSTALL_BETA="$answer"
    break
  fi
  echo "Invalid input. Please enter 'y' or 'n'."
done

# Timezone configuration
set_timezone Europe/Amsterdam

# Run OS updates if requested
if [ "$DO_UPDATES" == "y" ]; then
  update_os silent
fi

# Install dependencies
echo -e "${BLUE}►► Installing FFmpeg and alsa-utils...${NC}"
install_packages silent ffmpeg alsa-utils

# Stop existing service if running
if systemctl is-active --quiet encoder 2>/dev/null; then
  echo -e "${BLUE}►► Stopping existing encoder service...${NC}"
  systemctl stop encoder
fi

# Kill any rogue encoder processes not managed by systemd
if pkill -x encoder 2>/dev/null; then
  echo -e "${YELLOW}Killed orphaned encoder process(es)${NC}"
fi

# Create dedicated service user
if ! id -u encoder &>/dev/null; then
  echo -e "${BLUE}►► Creating encoder service user...${NC}"
  useradd --system --no-create-home --shell /usr/sbin/nologin --groups audio encoder
  echo -e "${GREEN}✓ User 'encoder' created with audio group membership${NC}"
else
  # Ensure existing user is in audio group
  if ! groups encoder | grep -q '\baudio\b'; then
    usermod -aG audio encoder
    echo -e "${GREEN}✓ Added 'encoder' user to audio group${NC}"
  fi
fi

# Get release version from GitHub API
echo -e "${BLUE}►► Fetching release information...${NC}"
if [ "$INSTALL_BETA" == "y" ]; then
  # Fetch the most recent prerelease
  LATEST_RELEASE=$(curl -s "https://api.github.com/repos/${GITHUB_REPO}/releases" | grep -E '"tag_name":|"prerelease":' | paste - - | grep 'true' | head -1 | sed -E 's/.*"tag_name": "([^"]+)".*/\1/')
  if [ -z "$LATEST_RELEASE" ]; then
    echo -e "${YELLOW}No beta/prerelease found, falling back to latest stable...${NC}"
    LATEST_RELEASE=$(curl -s "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
  else
    echo -e "${YELLOW}Installing prerelease version${NC}"
  fi
else
  # Fetch the latest stable release
  LATEST_RELEASE=$(curl -s "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
fi
if [ -z "$LATEST_RELEASE" ]; then
  echo -e "${RED}Failed to fetch release version${NC}"
  exit 1
fi
echo -e "${GREEN}✓ Version: ${LATEST_RELEASE}${NC}"

# Download encoder binary
echo -e "${BLUE}►► Downloading encoder binary...${NC}"
ENCODER_BINARY_URL="https://github.com/${GITHUB_REPO}/releases/download/${LATEST_RELEASE}/encoder-linux-arm64"
if ! curl -L -o "${INSTALL_DIR}/encoder" "$ENCODER_BINARY_URL"; then
  echo -e "${RED}Failed to download encoder binary${NC}"
  exit 1
fi
chmod +x "${INSTALL_DIR}/encoder"

# Create config directory with proper ownership
echo -e "${BLUE}►► Setting up configuration directory...${NC}"
mkdir -p "$CONFIG_DIR"
chown encoder:encoder "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"

# Note: Log directory /var/log/encoder is managed by systemd via LogsDirectory=

# Migrate config from old location if it exists
OLD_CONFIG="${INSTALL_DIR}/config.json"
NEW_CONFIG="${CONFIG_DIR}/config.json"
if [ -f "$OLD_CONFIG" ] && [ ! -f "$NEW_CONFIG" ]; then
  echo -e "${BLUE}►► Migrating config from old location...${NC}"
  mv "$OLD_CONFIG" "$NEW_CONFIG"
  echo -e "${GREEN}✓ Config migrated to ${NEW_CONFIG}${NC}"
fi

# Ensure config file has correct ownership
if [ -f "$NEW_CONFIG" ]; then
  chown encoder:encoder "$NEW_CONFIG"
  chmod 600 "$NEW_CONFIG"
fi

# Download and install systemd service
echo -e "${BLUE}►► Installing systemd service...${NC}"
if ! curl -s -o "$SERVICE_PATH" "$ENCODER_SERVICE_URL"; then
  echo -e "${RED}Failed to download service file${NC}"
  exit 1
fi

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable encoder

# Start the service
echo -e "${BLUE}►► Starting encoder service...${NC}"
systemctl start encoder

# Wait for service to start
sleep 2

# Verify installation
if ! systemctl is-active --quiet encoder; then
  echo -e "${RED}Warning: Encoder service failed to start. Check logs with: journalctl -u encoder${NC}"
else
  echo -e "${GREEN}✓ Encoder service is running${NC}"
fi

# Set up heartbeat monitoring if enabled
if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  echo -e "${BLUE}►► Setting up heartbeat monitoring...${NC}"
  HEARTBEAT_CRONJOB="* * * * * curl -fsS --max-time 10 -o /dev/null '$HEARTBEAT_URL' > /dev/null 2>&1"
  if ! crontab -l 2>/dev/null | grep -F -- "$HEARTBEAT_URL" > /dev/null; then
    (crontab -l 2>/dev/null; echo "$HEARTBEAT_CRONJOB") | crontab -
    echo -e "${GREEN}✓ Heartbeat monitoring configured${NC}"
  else
    echo -e "${YELLOW}Heartbeat monitoring already configured${NC}"
  fi
fi

# Completion message
echo -e "\n${GREEN}✓ Installation complete!${NC}"
echo -e "Open the web interface: ${BOLD}http://${FIRST_IP}:8080${NC}"
echo -e "Default credentials: ${BOLD}admin${NC} / ${BOLD}encoder${NC}"
echo -e "\nConfigure your SRT outputs via the web interface."
echo -e "The encoder will auto-start once you add at least one output.\n"
