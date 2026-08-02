# Bluetooth Printer Connection Recovery

## Printer mapping

The printers use the same advertised name (`BlueTooth Printer`), so always
identify them by MAC address:

| Printer | MAC address | Connection | Pairing |
| --- | --- | --- | --- |
| Cosina | `5A:4A:95:56:6F:B6` | Classic RFCOMM/SPP | PIN `1234` |
| Kitchen | `5A:4A:3E:3D:02:0A` | BLE | No pairing required |

The print agent should resolve them to these endpoints:

```text
Cosina  -> rfcomm://5A:4A:95:56:6F:B6
Kitchen -> ble://5A:4A:3E:3D:02:0A
```

## What happened

Both printers are dual-mode devices that advertise classic Bluetooth and BLE.
BlueZ sometimes selected the wrong bearer, which produced misleading errors:

```text
No more profiles to connect to
AuthenticationFailed
```

Multiple programs were also scanning or reconnecting concurrently, including
Ubuntu Bluetooth Settings, `bluetoothctl`, and the print agent. This prevented
the classic-only discovery filter from taking effect consistently.

Cosina requires classic RFCOMM pairing with PIN `1234`. Kitchen works directly
over BLE without a persistent bond.

## Fix that worked

1. Stop the print agent.
2. Close Ubuntu Bluetooth Settings so it does not continue scanning.
3. Remove Cosina's stale BlueZ record.
4. Discover Cosina using classic Bluetooth only.
5. Pair it and enter PIN `1234` when the PIN prompt appears.
6. Mark Cosina as trusted.
7. Connect Kitchen over BLE.
8. Restart the print agent and reconnect both printer workers.

## Quick recovery procedure

Close the Ubuntu Bluetooth Settings window, then stop the print agent if it is
running:

```sh
pkill -f 'go run ./cmd/print-agent'
```

Open the BlueZ command-line client:

```sh
bluetoothctl
```

Run the following commands inside `bluetoothctl`:

```text
agent KeyboardOnly
default-agent
remove 5A:4A:95:56:6F:B6
scan bredr
```

Wait until `5A:4A:95:56:6F:B6` appears, then run:

```text
scan off
pair 5A:4A:95:56:6F:B6
```

Enter `1234` when this prompt appears:

```text
[agent] Enter PIN code:
```

Do not type the PIN before the prompt. After pairing succeeds, run:

```text
trust 5A:4A:95:56:6F:B6
connect 5A:4A:3E:3D:02:0A
info 5A:4A:95:56:6F:B6
info 5A:4A:3E:3D:02:0A
quit
```

Both devices should report:

```text
Connected: yes
```

Cosina should also report:

```text
Paired: yes
Bonded: yes
Trusted: yes
```

Restart the print agent from the repository root:

```sh
go run ./cmd/print-agent
```

Open the dashboard at <http://127.0.0.1:17432/admin> and confirm both printer
states are `connected`. Use **Reconnect all** if their Ubuntu connections are
already active but the dashboard has not refreshed.

## Normal future recovery

Do not remove Cosina from Ubuntu after it has been paired. Its stored bond and
PIN should allow automatic RFCOMM reconnection. For routine disconnects:

1. Confirm both printers are powered on and nearby.
2. Power-cycle the disconnected printer.
3. Use **Reconnect all** in the dashboard.
4. Use the full recovery procedure only if Cosina has lost its bond or BlueZ
   again selects the wrong bearer.

## Setup-page controls and limitations

The Linux setup page displays both the configured default (`Auto`, `RFCOMM`,
or `BLE`) and the protocol currently used by the Print Agent. Protocol choice
is per printer; it does not change Ubuntu's system-wide Bluetooth policy.

GNOME Bluetooth Settings scans continuously while its panel is open. Close it
before a protocol-specific scan, or run:

```sh
pkill -f '^/usr/bin/gnome-control-center bluetooth$'
```

This command does not need administrator access and does not stop BlueZ. Other
Linux desktops require closing their Bluetooth settings window manually.
Never stop `bluetooth.service`, because the Print Agent requires it.

**Disconnect now** is deliberately one-shot. If automatic reconnection is
enabled, the printer can reconnect immediately. Disconnecting a BLE device
also affects other local applications using that device. **Forget** removes
the Ubuntu bond and is unavailable until the printer is removed from the
Print Agent. BlueZ management normally works in a logged-in desktop session;
restricted D-Bus/Polkit installations may require using the system Bluetooth
panel or `bluetoothctl` manually.
