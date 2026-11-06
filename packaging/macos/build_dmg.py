#!/usr/bin/env python3
"""Build KubeDeck.app (universal: Apple Silicon + Intel) and a drag-to-install DMG.

Works on Linux and macOS.
  Linux needs: go, xorrisofs, `dmg` from libdmg-hfsplus, rcodesign, `pip install ds_store`
  macOS needs: go (uses lipo/codesign/hdiutil from the system)

  python3 packaging/macos/build_dmg.py --version 1.0.0 --out dist
"""
import argparse
import os
import plistlib
import shutil
import struct
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
APP = "KubeDeck"
BUNDLE_ID = "com.zanayeem.kubedeck"


def run(*cmd, **kw):
    print("  $", " ".join(cmd))
    subprocess.run(cmd, check=True, **kw)


def go_build(arch, out):
    env = dict(os.environ, GOOS="darwin", GOARCH=arch, CGO_ENABLED="0")
    run("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", out, ".", cwd=ROOT, env=env)


def lipo(inputs, out):
    """Write a universal (fat) Mach-O. Pure Python so it works on Linux."""
    if shutil.which("lipo") and sys.platform == "darwin":
        run("lipo", "-create", "-output", out, *inputs)
        return
    slices = []
    for p in inputs:
        data = open(p, "rb").read()
        magic, cputype, cpusub = struct.unpack_from("<III", data, 0)
        assert magic == 0xFEEDFACF, f"{p} is not a 64-bit Mach-O"
        slices.append((cputype, cpusub & 0x00FFFFFF, data))
    align = 14  # 16 KiB pages, required for arm64
    A = 1 << align
    roundup = lambda n: (n + A - 1) & ~(A - 1)
    offsets, off = [], A  # the fat header is tiny, so the first slice starts at 16 KiB
    for _, _, data in slices:
        offsets.append(off)
        off = roundup(off + len(data))
    out_bytes = bytearray(struct.pack(">II", 0xCAFEBABE, len(slices)))
    for (cputype, cpusub, data), o in zip(slices, offsets):
        out_bytes += struct.pack(">IIIII", cputype, cpusub, o, len(data), align)
    for (_, _, data), o in zip(slices, offsets):
        out_bytes += b"\0" * (o - len(out_bytes))
        out_bytes += data
    with open(out, "wb") as f:
        f.write(out_bytes)
    os.chmod(out, 0o755)


def make_bundle(path, binary, version):
    c = os.path.join(path, "Contents")
    os.makedirs(os.path.join(c, "MacOS"))
    os.makedirs(os.path.join(c, "Resources"))
    shutil.copy2(binary, os.path.join(c, "MacOS", APP))
    os.chmod(os.path.join(c, "MacOS", APP), 0o755)
    shutil.copy2(os.path.join(HERE, "KubeDeck.icns"), os.path.join(c, "Resources", "KubeDeck.icns"))
    info = {
        "CFBundleName": APP,
        "CFBundleDisplayName": APP,
        "CFBundleIdentifier": BUNDLE_ID,
        "CFBundleExecutable": APP,
        "CFBundleIconFile": "KubeDeck",
        "CFBundlePackageType": "APPL",
        "CFBundleSignature": "????",
        "CFBundleShortVersionString": version,
        "CFBundleVersion": version,
        "CFBundleInfoDictionaryVersion": "6.0",
        "CFBundleDevelopmentRegion": "en",
        "LSMinimumSystemVersion": "11.0",
        # KubeDeck is a background server with a browser UI: no Dock icon or menu bar.
        "LSUIElement": True,
        "LSApplicationCategoryType": "public.app-category.developer-tools",
        "NSHumanReadableCopyright": "MIT licence · github.com/zanayeem",
        "NSHighResolutionCapable": True,
    }
    with open(os.path.join(c, "Info.plist"), "wb") as f:
        plistlib.dump(info, f)
    with open(os.path.join(c, "PkgInfo"), "w") as f:
        f.write("APPL????")


def sign(app):
    if sys.platform == "darwin" and shutil.which("codesign"):
        run("codesign", "--force", "--deep", "--sign", "-", "--identifier", BUNDLE_ID, app)
        return
    rc = shutil.which("rcodesign") or os.environ.get("RCODESIGN")
    if not rc:
        print("  ! rcodesign not found: the app will be unsigned (Apple Silicon Macs refuse unsigned apps)")
        return
    # no certificate given → ad-hoc signature (what `codesign -s -` produces)
    run(rc, "sign", "--binary-identifier", f"Contents/MacOS/{APP}={BUNDLE_ID}", app)


README = """KubeDeck {version}
================

Install: drag KubeDeck onto the Applications folder.

Open: double-click KubeDeck. It runs in the background (no Dock icon) and opens
http://127.0.0.1:7080 in your browser. It reads ~/.kube/config; change the path
on the "All clusters" page. Open the app again any time to bring the page back.
Stop it with the "Stop server" button at the top-right of the page.
The Terminal button opens a shell with kubectl set to the cluster you're viewing.

If macOS says it "could not verify" KubeDeck
--------------------------------------------
KubeDeck is open source and not notarised by Apple, so the first launch is blocked:
  1. Click "Done" in the warning.
  2. Open System Settings → Privacy & Security, scroll down.
  3. Next to "KubeDeck was blocked…", click "Open Anyway", then confirm.
Or, in Terminal:  xattr -dr com.apple.quarantine /Applications/KubeDeck.app

EKS / exec auth: KubeDeck uses your shell's PATH, so `aws`, `kubelogin`
and `gke-gcloud-auth-plugin` from Homebrew are found.

Logs: ~/Library/Logs/KubeDeck.log
"""


def write_ds_store(root, version):
    try:
        from ds_store import DSStore
    except ImportError:
        print("  ! ds_store not installed: Finder will use its default layout")
        return
    with DSStore.open(os.path.join(root, ".DS_Store"), "w+") as d:
        d["."]["bwsp"] = {
            "ShowStatusBar": False, "ShowToolbar": False, "ShowTabView": False, "ShowPathbar": False,
            "ShowSidebar": False, "ContainerShowSidebar": False, "SidebarWidth": 0,
            "WindowBounds": "{{240, 160}, {640, 420}}",
        }
        d["."]["icvp"] = {
            "viewOptionsVersion": 1, "arrangeBy": "none", "iconSize": 112.0, "textSize": 13.0,
            "labelOnBottom": True, "showIconPreview": True, "showItemInfo": False,
            "gridOffsetX": 0.0, "gridOffsetY": 0.0, "gridSpacing": 100.0,
            "scrollPositionX": 0.0, "scrollPositionY": 0.0,
            "backgroundType": 0,
            "backgroundColorRed": 1.0, "backgroundColorGreen": 1.0, "backgroundColorBlue": 1.0,
        }
        d["."]["vSrn"] = ("long", 1)
        d[f"{APP}.app"]["Iloc"] = (170, 170)
        d["Applications"]["Iloc"] = (470, 170)
        d["Read me first.txt"]["Iloc"] = (320, 320)


def make_dmg(stage, out, volname):
    if sys.platform == "darwin" and shutil.which("hdiutil"):
        run("hdiutil", "create", "-volname", volname, "-srcfolder", stage, "-ov", "-format", "UDZO", out)
        return
    iso = out + ".iso"
    run("xorrisofs", "-quiet", "-D", "-l", "-V", volname, "-no-pad", "-r", "-dir-mode", "0755", "-o", iso, stage)
    dmg = shutil.which("dmg") or os.environ.get("DMG_TOOL")
    if not dmg:
        sys.exit("libdmg-hfsplus `dmg` tool not found (set DMG_TOOL)")
    run(dmg, iso, out)
    os.remove(iso)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True)
    ap.add_argument("--out", default=os.path.join(ROOT, "dist"))
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp:
        print("1/5 building darwin/arm64 and darwin/amd64")
        arm, amd = os.path.join(tmp, "kd-arm64"), os.path.join(tmp, "kd-amd64")
        go_build("arm64", arm)
        go_build("amd64", amd)
        print("2/5 merging into a universal binary")
        uni = os.path.join(tmp, "kd-universal")
        lipo([arm, amd], uni)
        print("3/5 assembling KubeDeck.app")
        stage = os.path.join(tmp, "stage")
        os.makedirs(stage)
        app = os.path.join(stage, f"{APP}.app")
        make_bundle(app, uni, a.version)
        print("4/5 signing (ad-hoc)")
        sign(app)
        os.symlink("/Applications", os.path.join(stage, "Applications"))
        with open(os.path.join(stage, "Read me first.txt"), "w") as f:
            f.write(README.format(version=a.version))
        write_ds_store(stage, a.version)
        print("5/5 creating the DMG")
        out = os.path.join(a.out, f"KubeDeck-{a.version}-macOS-universal.dmg")
        if os.path.exists(out):
            os.remove(out)
        make_dmg(stage, out, APP)
    print("done:", out)


if __name__ == "__main__":
    main()
