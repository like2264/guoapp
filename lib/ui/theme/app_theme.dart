import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import 'motion.dart';

abstract final class AppTheme {
  static ThemeMode mode(String preference) => switch (preference) {
    'light' => ThemeMode.light,
    'dark' => ThemeMode.dark,
    _ => ThemeMode.system,
  };

  static SystemUiOverlayStyle systemBars(Brightness brightness) {
    final icons = brightness == Brightness.dark
        ? Brightness.light
        : Brightness.dark;
    return SystemUiOverlayStyle(
      statusBarColor: Colors.transparent,
      statusBarIconBrightness: icons,
      statusBarBrightness: brightness,
      systemStatusBarContrastEnforced: false,
      systemNavigationBarColor: Colors.transparent,
      systemNavigationBarDividerColor: Colors.transparent,
      systemNavigationBarIconBrightness: icons,
      systemNavigationBarContrastEnforced: false,
    );
  }

  static ThemeData build(
    Color seed,
    Brightness brightness, {
    required bool television,
    bool pureBlack = false,
    bool liquidGlass = false,
  }) {
    final seeded = ColorScheme.fromSeed(
      seedColor: seed,
      brightness: brightness,
    );
    final scheme = pureBlack && brightness == Brightness.dark
        ? seeded.copyWith(
            surface: Colors.black,
            surfaceDim: Colors.black,
            surfaceContainerLowest: Colors.black,
            surfaceContainerLow: const Color(0xFF0B0B0B),
            surfaceContainer: const Color(0xFF121212),
            surfaceContainerHigh: const Color(0xFF1A1A1A),
            surfaceContainerHighest: const Color(0xFF242424),
          )
        : seeded;
    final glass = liquidGlass ? _glassScheme(seeded) : null;
    final theme = ThemeData(
      useMaterial3: true,
      colorScheme: glass ?? scheme,
      scaffoldBackgroundColor: (glass ?? scheme).surface,
      pageTransitionsTheme: pageTransitions(television: television),
      appBarTheme: AppBarTheme(
        backgroundColor: scheme.surface,
        foregroundColor: scheme.onSurface,
        scrolledUnderElevation: 0,
        centerTitle: false,
        systemOverlayStyle: systemBars(brightness),
      ),
      dividerTheme: DividerThemeData(
        color: scheme.outlineVariant,
        space: 1,
        thickness: 1,
      ),
    );
    final titled = theme.copyWith(
      appBarTheme: theme.appBarTheme.copyWith(
        titleTextStyle: theme.textTheme.titleLarge?.copyWith(
          fontSize: 20,
          fontWeight: FontWeight.w700,
          color: scheme.onSurface,
        ),
      ),
      navigationBarTheme: NavigationBarThemeData(
        labelTextStyle: WidgetStateProperty.resolveWith((states) {
          final selected = states.contains(WidgetState.selected);
          return theme.textTheme.labelMedium?.copyWith(
            fontWeight: selected ? FontWeight.w700 : FontWeight.w500,
            color: selected ? scheme.onSurface : scheme.onSurfaceVariant,
          );
        }),
      ),
    );
    final glassed = glass == null
        ? titled
        : _liquidGlass(titled, glass, brightness == Brightness.dark);
    return television ? _television(glassed) : glassed;
  }

  // Liquid Glass 的底色：比默认浅/深一档，容器留出半透明的“玻璃层”空间。
  static ColorScheme _glassScheme(ColorScheme scheme) {
    final dark = scheme.brightness == Brightness.dark;
    return scheme.copyWith(
      surface: dark ? const Color(0xFF12161E) : const Color(0xFFF4F6FB),
      surfaceDim: dark ? const Color(0xFF0C0F15) : const Color(0xFFE9ECF4),
      surfaceContainerLowest: dark
          ? const Color(0xFF0E1219)
          : const Color(0xFFFFFFFF),
      surfaceContainerLow: dark
          ? const Color(0xFF161B24)
          : const Color(0xFFFAFBFE),
      surfaceContainer: dark
          ? const Color(0xFF1B212B)
          : const Color(0xFFF1F4FA),
      surfaceContainerHigh: dark
          ? const Color(0xFF232A36)
          : const Color(0xFFEAEEF6),
      surfaceContainerHighest: dark
          ? const Color(0xFF2C3442)
          : const Color(0xFFE1E6F0),
      outline: scheme.outline.withValues(alpha: 0.55),
      outlineVariant: scheme.outlineVariant.withValues(alpha: 0.35),
    );
  }

  static ThemeData _liquidGlass(ThemeData theme, ColorScheme colors, bool dark) {
    final fill = dark
        ? Colors.white.withValues(alpha: 0.06)
        : Colors.white.withValues(alpha: 0.74);
    final hairline = dark
        ? Colors.white.withValues(alpha: 0.14)
        : Colors.white.withValues(alpha: 0.85);
    final pill = RoundedRectangleBorder(
      borderRadius: BorderRadius.circular(26),
      side: BorderSide(color: hairline),
    );
    final button = ButtonStyle(
      shape: WidgetStatePropertyAll(pill),
      elevation: const WidgetStatePropertyAll<double>(0),
    );
    return theme.copyWith(
      appBarTheme: theme.appBarTheme.copyWith(
        backgroundColor: Colors.transparent,
        elevation: 0,
        scrolledUnderElevation: 0,
      ),
      navigationBarTheme: theme.navigationBarTheme.copyWith(
        backgroundColor: fill,
        elevation: 0,
        shadowColor: Colors.transparent,
        indicatorColor: colors.primary.withValues(alpha: dark ? 0.24 : 0.16),
      ),
      bottomSheetTheme: BottomSheetThemeData(
        backgroundColor: fill,
        modalBackgroundColor: fill,
        elevation: 0,
        modalElevation: 0,
        shape: RoundedRectangleBorder(
          borderRadius: const BorderRadius.vertical(top: Radius.circular(28)),
          side: BorderSide(color: hairline),
        ),
      ),
      filledButtonTheme: FilledButtonThemeData(style: button),
      outlinedButtonTheme: OutlinedButtonThemeData(style: button),
      textButtonTheme: TextButtonThemeData(style: button),
    );
  }

  static ThemeData _television(ThemeData theme) {
    final colors = theme.colorScheme;
    final button = ButtonStyle(
      overlayColor: WidgetStateProperty.resolveWith<Color?>(
        (states) => states.contains(WidgetState.focused)
            ? colors.onSurface.withValues(alpha: 0.12)
            : null,
      ),
      minimumSize: const WidgetStatePropertyAll(Size(52, 48)),
    );
    return theme.copyWith(
      materialTapTargetSize: MaterialTapTargetSize.shrinkWrap,
      focusColor: colors.primary.withValues(alpha: 0.16),
      iconButtonTheme: IconButtonThemeData(style: button),
      filledButtonTheme: FilledButtonThemeData(style: button),
      outlinedButtonTheme: OutlinedButtonThemeData(style: button),
      textButtonTheme: TextButtonThemeData(style: button),
    );
  }
}
