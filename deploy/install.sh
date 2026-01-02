#!/usr/bin/env bash
set -euo pipefail

# Configuration
GITHUB_REPO="oszuidwest/zwfm-encoder"
ENCODER_SERVICE_URL="https://raw.githubusercontent.com/${GITHUB_REPO}/main/deploy/encoder.service"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/encoder"
CONFIG_FILE="${CONFIG_DIR}/config.json"
SERVICE_PATH="/etc/systemd/system/encoder.service"

# Functions library (v2)
FUNCTIONS_LIB_PATH=$(mktemp)
FUNCTIONS_LIB_URL="https://raw.githubusercontent.com/oszuidwest/bash-functions/v2/common-functions.sh"

# Clean up temporary file on exit
trap 'rm -f "$FUNCTIONS_LIB_PATH"' EXIT

# General Raspberry Pi configuration
CONFIG_TXT_PATHS=("/boot/firmware/config.txt" "/boot/config.txt")
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
assert_user_privileged "root"
assert_os_linux
assert_os_64bit
assert_hw_rpi 4

# Determine the correct config.txt path
CONFIG_TXT=""
for path in "${CONFIG_TXT_PATHS[@]}"; do
  if [ -f "$path" ]; then
    CONFIG_TXT="$path"
    break
  fi
done

if [ -z "$CONFIG_TXT" ]; then
  echo -e "${RED}Error: config.txt not found in known locations.${NC}"
  exit 1
fi

# Check if the required tools are installed
assert_tool curl systemctl

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
if ! grep -q "^dtoverlay=hifiberry" "$CONFIG_TXT"; then
  echo -e "${RED}No HiFiBerry card configured in the $CONFIG_TXT file. Exiting...${NC}\n" >&2
  exit 1
fi

# =============================================================================
# CONFIGURATION QUESTIONS (all upfront)
# =============================================================================

echo -e "${BLUE}►► Configuration${NC}\n"

# Station name
prompt_user "STATION_NAME" "ZuidWest FM" "Enter your station name" "str"

# Web username
prompt_user "WEB_USERNAME" "admin" "Enter the web interface username" "str"

# Web password
prompt_user "WEB_PASSWORD" "encoder" "Enter the web interface password" "str"

# Web port
prompt_user "WEB_PORT" "8080" "Enter the web interface port" "num"

# Timezone with validation (retry loop)
while true; do
  prompt_user "TIMEZONE" "Europe/Amsterdam" "Enter your timezone (e.g. Europe/Amsterdam, Europe/London, Europe/Berlin)" "str"
  if timedatectl list-timezones | grep -qx "$TIMEZONE"; then
    break
  fi
  echo -e "${RED}Invalid timezone '${TIMEZONE}'. Use 'timedatectl list-timezones' to see valid options.${NC}"
done

echo ""

# OS updates
prompt_user "DO_UPDATES" "y" "Do you want to perform all OS updates? (y/n)" "y/n"

# Heartbeat monitoring
prompt_user "ENABLE_HEARTBEAT" "n" "Do you want to enable heartbeat monitoring via UptimeRobot? (y/n)" "y/n"

HEARTBEAT_URL=""
if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  prompt_user "HEARTBEAT_URL" "" "Enter the heartbeat URL to ping every minute" "str"
fi

# Beta version
prompt_user "INSTALL_BETA" "n" "Do you want to install a beta/prerelease version? (y/n)" "y/n"

# =============================================================================
# SUMMARY AND CONFIRMATION
# =============================================================================

echo -e "\n${BLUE}►► Installation Summary${NC}\n"
echo -e "Station name:     ${BOLD}${STATION_NAME}${NC}"
echo -e "Web username:     ${BOLD}${WEB_USERNAME}${NC}"
echo -e "Web password:     ${BOLD}********${NC}"
echo -e "Web port:         ${BOLD}${WEB_PORT}${NC}"
echo -e "Timezone:         ${BOLD}${TIMEZONE}${NC}"
echo -e "OS updates:       ${BOLD}${DO_UPDATES}${NC}"
echo -e "Heartbeat:        ${BOLD}${ENABLE_HEARTBEAT}${NC}"
if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  echo -e "Heartbeat URL:    ${BOLD}${HEARTBEAT_URL}${NC}"
fi
echo -e "Beta version:     ${BOLD}${INSTALL_BETA}${NC}"
echo -e "Config location:  ${BOLD}${CONFIG_FILE}${NC}"

echo ""
prompt_user "CONFIRM" "y" "Continue with installation? (y/n)" "y/n"

if [ "$CONFIRM" != "y" ]; then
  echo -e "${YELLOW}Installation cancelled.${NC}"
  exit 0
fi

# =============================================================================
# INSTALLATION
# =============================================================================

echo -e "\n${BLUE}►► Starting installation...${NC}\n"

# Set timezone
set_timezone "$TIMEZONE"

# Run OS updates if requested
if [ "$DO_UPDATES" == "y" ]; then
  apt_update --silent
fi

# Install dependencies (including jq for config generation)
echo -e "${BLUE}►► Installing FFmpeg, alsa-utils, and jq...${NC}"
apt_install --silent ffmpeg alsa-utils jq

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
ENCODER_BINARY_URL="https://github.com/${GITHUB_REPO}/releases/download/${LATEST_RELEASE}/encoder-linux-arm64"
file_download "$ENCODER_BINARY_URL" "${INSTALL_DIR}/encoder" "encoder binary"
chmod +x "${INSTALL_DIR}/encoder"

# Create config directory with proper ownership
echo -e "${BLUE}►► Setting up configuration directory...${NC}"
mkdir -p "$CONFIG_DIR"
chown encoder:encoder "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"

# Migrate config from old location if it exists
OLD_CONFIG="${INSTALL_DIR}/config.json"
if [ -f "$OLD_CONFIG" ] && [ ! -f "$CONFIG_FILE" ]; then
  echo -e "${BLUE}►► Migrating config from old location...${NC}"
  mv "$OLD_CONFIG" "$CONFIG_FILE"
  chown encoder:encoder "$CONFIG_FILE"
  chmod 600 "$CONFIG_FILE"
  echo -e "${GREEN}✓ Config migrated to ${CONFIG_FILE}${NC}"
fi

# Backup existing config if present
if [ -f "$CONFIG_FILE" ]; then
  echo -e "${BLUE}►► Backing up existing configuration...${NC}"
  file_backup "$CONFIG_FILE"
fi

# Generate configuration file using jq (minimal config, app fills defaults)
echo -e "${BLUE}►► Generating configuration file...${NC}"
jq -n \
  --arg station_name "$STATION_NAME" \
  --arg username "$WEB_USERNAME" \
  --arg password "$WEB_PASSWORD" \
  --argjson port "$WEB_PORT" \
  '{
    station: { name: $station_name },
    web: { port: $port, username: $username, password: $password }
  }' > "$CONFIG_FILE"

# Set proper ownership and permissions
chown encoder:encoder "$CONFIG_FILE"
chmod 600 "$CONFIG_FILE"
echo -e "${GREEN}✓ Configuration saved to ${CONFIG_FILE}${NC}"

# Download and install systemd service
file_download "$ENCODER_SERVICE_URL" "$SERVICE_PATH" "systemd service"

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
  echo -e "${RED}Warning: Encoder service failed to start.${NC}"
  echo -e "Check logs with: ${BOLD}journalctl -u encoder -n 50${NC}"
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

# =============================================================================
# COMPLETION
# =============================================================================

echo -e "\n${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${GREEN}✓ Installation complete!${NC}"
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"

echo -e "\n${BOLD}Web Interface${NC}"
echo -e "  URL:      ${BOLD}http://${FIRST_IP}:${WEB_PORT}${NC}"
echo -e "  Username: ${BOLD}${WEB_USERNAME}${NC}"
echo -e "  Password: ${BOLD}(as configured)${NC}"

echo -e "\n${BOLD}Next Steps${NC}"
echo -e "  1. Open the web interface in your browser"
echo -e "  2. Select your audio input device in Settings"
echo -e "  3. Add at least one SRT output destination"

echo -e "\n${BOLD}Useful Commands${NC}"
echo -e "  View logs:      ${BOLD}journalctl -u encoder -f${NC}"
echo -e "  Restart:        ${BOLD}systemctl restart encoder${NC}"
echo -e "  Edit config:    ${BOLD}nano ${CONFIG_FILE}${NC}"
echo ""
