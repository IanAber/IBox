#!/bin/bash
if [ "$USER" != "root" ]
then
    echo "Please run this as root or with sudo"
    exit 2
fi

systemctl stop iBox
cp bin/ARM/iBox /usr/local/bin/iBox
install -m 755 ibox-reset-serial.sh /usr/local/sbin/ibox-reset-serial

# Install/refresh the unit so service-file changes (e.g. the config path) take effect.
cp iBox.service /etc/systemd/system/iBox.service
systemctl daemon-reload

# One-time move of the circuit configuration from its old location.
OLD_CONFIG=/home/egadmin/circuit-config.json
NEW_CONFIG=/etc/ibox.config
if [ ! -e "$NEW_CONFIG" ] && [ -e "$OLD_CONFIG" ]
then
    cp -p "$OLD_CONFIG" "$NEW_CONFIG"
    mv "$OLD_CONFIG" "$OLD_CONFIG.migrated"
    echo "Moved $OLD_CONFIG to $NEW_CONFIG (old copy kept as $OLD_CONFIG.migrated)"
fi

mkdir -p /var/www/html
cp -r web/* /var/www/html
systemctl start iBox
