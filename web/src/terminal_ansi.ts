// Fixed agent-terminal ANSI mapping with hue-preserving bright neighbours.
// Product chrome colors come only from tokens.css. No daemon overrides.
import type { ITheme } from "@xterm/xterm";
export const TERMINAL_ANSI: Record<"light" | "dark", ITheme> = {
  "light": {
    "black": "#2E3440",
    "red": "#944049",
    "green": "#51693C",
    "yellow": "#7C5A15",
    "blue": "#426486",
    "magenta": "#7F5478",
    "cyan": "#2D6271",
    "white": "#434C5E",
    "brightBlack": "#000000",
    "brightRed": "#76333A",
    "brightGreen": "#415430",
    "brightYellow": "#634811",
    "brightBlue": "#35506B",
    "brightMagenta": "#664360",
    "brightCyan": "#244E5A",
    "brightWhite": "#171A20"
  },
  "dark": {
    "black": "#B4BCC8",
    "red": "#D9B2B9",
    "green": "#A3BE8C",
    "yellow": "#EBCB8B",
    "blue": "#81A1C1",
    "magenta": "#B590AF",
    "cyan": "#90C4D3",
    "white": "#D8DEE9",
    "brightBlack": "#959CA5",
    "brightRed": "#E1C1C7",
    "brightGreen": "#B5CBA3",
    "brightYellow": "#EFD5A2",
    "brightBlue": "#9AB4CD",
    "brightMagenta": "#C4A6BF",
    "brightCyan": "#A6D0DC",
    "brightWhite": "#FFFFFF"
  }
};
