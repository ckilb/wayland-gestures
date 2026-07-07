# wayland-gestures

A tiny Go daemon that detects a **right-mouse-button swipe to the right** and
launches a command (`dolphin` by default).

It reads raw input from the Linux **evdev** interface (`/dev/input/event*`), so
it works **globally on Wayland and X11**, no matter which window has focus and
without any compositor-specific APIs.

## Gesture

It works with both a real mouse and a clickpad/trackpad (e.g. a MacBook's).

**Mouse:**
1. Press and hold the **right button**.
2. Move **to the right**.
3. Release.

**Trackpad / clickpad** (no hardware right button — right-click is a two-finger
click, exactly like libinput's default "clickfinger" rule):
1. Put **two fingers** on the pad and **press it down** (physical click).
2. Slide **to the right**.
3. Release.

In both cases vertical wobble is tolerated, so the motion does not need to be a
perfectly straight horizontal line.

## Build

```sh
go build -o wayland-gestures .
```

## Permissions (required)

Reading `/dev/input/event*` requires membership in the `input` group (the device
nodes are `root:input`). Add yourself once, then log out and back in:

```sh
sudo usermod -aG input "$USER"
# log out / back in (or: newgrp input) for the group to take effect
```

Verify with `groups | tr ' ' '\n' | grep input`.

> Alternatively run it with `sudo ./wayland-gestures`, but the group approach is
> preferred so it can run as your normal user / on login.

## Run

```sh
./wayland-gestures
```

You should see a `monitoring mouse: ...` line for each detected pointer. Perform
the gesture and `dolphin` launches.

## Options

```
-cmd string        command to launch (default "dolphin")
-dist float        MOUSE: minimum rightward travel in evdev units (default 60)
-tpdist float      TOUCHPAD: minimum rightward travel as a fraction of pad
                   width, 0..1 (default 0.15)
-tol float         vertical tolerance as a fraction of horizontal travel,
                   0 = perfectly straight (default 0.7)
-cooldown duration minimum time between launches (default 800ms)
```

Anything after the flags is passed as arguments to the command, e.g.:

```sh
./wayland-gestures -cmd dolphin /home/chris
./wayland-gestures -dist 100 -tol 0.4 -cmd firefox
```

Tuning tips:
- Gesture triggers too easily → increase `-dist` (mouse) / `-tpdist` (trackpad).
- Trackpad needs a shorter slide → decrease `-tpdist` (e.g. `0.1`).
- Diagonal swipes get rejected → increase `-tol`.
- Want a stricter straight line → decrease `-tol`.

## Run automatically on login (systemd user service)

Create `~/.config/systemd/user/wayland-gestures.service`:

```ini
[Unit]
Description=Right-swipe mouse gesture launcher

[Service]
ExecStart=%h/Sites/wayland-gestures/wayland-gestures
Restart=on-failure

[Install]
WantedBy=default.target
```

Then:

```sh
systemctl --user daemon-reload
systemctl --user enable --now wayland-gestures.service
journalctl --user -u wayland-gestures.service -f   # view logs
```

(The `input` group membership still applies to the user the service runs as.)

## How it works

- Enumerates `/dev/input/event*` and classifies each device via the
  `EVIOCGBIT` ioctl: relative motion (`EV_REL`) → mouse; absolute motion
  (`EV_ABS`) with a valid `ABS_X` range → touchpad/clickpad.
- Each device is read in its own goroutine. The list is re-scanned every 3s, so
  a device plugged in later is picked up automatically.
- **Mouse:** on `BTN_RIGHT` press it accumulates `REL_X`/`REL_Y` until release,
  then checks `dx >= -dist` and `|dy| <= dx * -tol`.
- **Clickpad:** it tracks finger count (`BTN_TOOL_FINGER`/`DOUBLETAP`/`TRIPLETAP`)
  and absolute position (`ABS_X`/`ABS_Y`). A `BTN_LEFT` press with two fingers
  down is treated as a right-click; on release it checks the horizontal travel
  against `-tpdist` (a fraction of the pad width, read via `EVIOCGABS`) and
  `-tol`. A one-finger click (left click) and a two-finger scroll (no click) are
  ignored.

## Notes / limitations

- Mouse movement is measured in evdev relative units (affected by mouse
  resolution), not screen pixels, so `-dist` may need tuning.
- Trackpad right-click is assumed to be a two-finger click (libinput's default).
  If you configured corner/areas right-click instead and your pad emits a real
  `BTN_RIGHT`, that is also handled.
