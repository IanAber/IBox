#!/bin/bash
if [ "$USER" != "root" ]
then
    echo "Please run this as root or with sudo"
    exit 2
fi

systemctl stop iBox
cp bin/ARM/iBox /usr/local/bin/iBox

mkdir -p /var/www/html
cp -r web/* /var/www/html
systemctl start iBox
