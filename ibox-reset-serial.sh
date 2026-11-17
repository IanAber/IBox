#!/bin/sh
# Reset the SC16IS7xx RS485 UART chip before iBox starts.
#
# After a port is closed (service stop/restart), the chip can be left in a
# state where the ports open but never receive data until a reboot.
# Unbinding and rebinding the kernel driver resets the chip the same way a
# reboot does. Run as root (ExecStartPre in iBox.service).

found=0
for drv in /sys/bus/spi/drivers/sc16is7xx /sys/bus/i2c/drivers/sc16is7xx; do
    [ -d "$drv" ] || continue
    for dev in "$drv"/*; do
        [ -L "$dev" ] || continue          # devices are symlinks; skip bind/unbind/uevent files
        name=$(basename "$dev")
        [ "$name" = module ] && continue   # link to the kernel module, not a device
        found=1
        echo "ibox-reset-serial: resetting $name"
        echo "$name" > "$drv/unbind" || echo "ibox-reset-serial: unbind $name failed" >&2
        sleep 1
        echo "$name" > "$drv/bind" || echo "ibox-reset-serial: bind $name failed" >&2
    done
done

if [ "$found" = 0 ]; then
    echo "ibox-reset-serial: no sc16is7xx device found; nothing to reset"
    exit 0
fi

# Wait for udev to recreate the device nodes.
i=0
while [ $i -lt 50 ]; do
    if [ -e /dev/ttySC0 ] && [ -e /dev/ttySC1 ]; then
        exit 0
    fi
    sleep 0.1
    i=$((i + 1))
done
echo "ibox-reset-serial: /dev/ttySC0 or /dev/ttySC1 did not reappear" >&2
exit 0
