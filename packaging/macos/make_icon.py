#!/usr/bin/env python3
"""Rebuild KubeLamp.icns from the 1024px icon (needs Pillow): python3 make_icon.py"""
import os
from PIL import Image
here = os.path.dirname(os.path.abspath(__file__))
Image.open(os.path.join(here, "icon-1024.png")).convert("RGBA").save(os.path.join(here, "KubeLamp.icns"), format="ICNS")
