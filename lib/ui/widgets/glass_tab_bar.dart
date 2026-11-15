import 'dart:ui';

import 'package:flutter/material.dart';

/// iOS 26 那种浮动玻璃标签栏：整条是半透明胶囊、背景做高斯模糊，
/// 选中项在条内再浮起一层更亮的胶囊（高光边 + 主题色投影），
/// 图标与文字跟随主题色，其余项保持低对比度。
class GlassTabBar extends StatelessWidget {
  const GlassTabBar({
    super.key,
    required this.selectedIndex,
    required this.onSelected,
    required this.destinations,
  });

  final int selectedIndex;
  final ValueChanged<int> onSelected;
  final List<GlassTabDestination> destinations;

  /// 胶囊本体高度（不含底部安全区）。
  static const barHeight = 62.0;

  @override
  Widget build(BuildContext context) {
    final dark = Theme.of(context).brightness == Brightness.dark;
    final colors = Theme.of(context).colorScheme;
    final inset = MediaQuery.viewPaddingOf(context).bottom;
    return Padding(
      padding: EdgeInsets.fromLTRB(14, 0, 14, 10 + inset),
      child: ClipRRect(
        borderRadius: BorderRadius.circular(34),
        child: BackdropFilter(
          filter: ImageFilter.blur(
            sigmaX: 26,
            sigmaY: 26,
            tileMode: TileMode.clamp,
          ),
          child: Container(
            height: barHeight,
            padding: const EdgeInsets.all(6),
            decoration: BoxDecoration(
              borderRadius: BorderRadius.circular(34),
              gradient: LinearGradient(
                begin: Alignment.topCenter,
                end: Alignment.bottomCenter,
                colors: dark
                    ? [
                        Colors.white.withValues(alpha: 0.15),
                        Colors.white.withValues(alpha: 0.06),
                      ]
                    : [
                        Colors.white.withValues(alpha: 0.92),
                        Colors.white.withValues(alpha: 0.66),
                      ],
              ),
              border: Border.all(
                color: dark
                    ? Colors.white.withValues(alpha: 0.18)
                    : Colors.white.withValues(alpha: 0.9),
              ),
              boxShadow: [
                BoxShadow(
                  color: Colors.black.withValues(alpha: dark ? 0.42 : 0.14),
                  blurRadius: 24,
                  offset: const Offset(0, 10),
                ),
              ],
            ),
            child: Row(
              children: [
                for (var index = 0; index < destinations.length; index++)
                  Expanded(child: _item(context, index, dark, colors)),
              ],
            ),
          ),
        ),
      ),
    );
  }

  Widget _item(
    BuildContext context,
    int index,
    bool dark,
    ColorScheme colors,
  ) {
    final destination = destinations[index];
    final selected = index == selectedIndex;
    final idle = dark
        ? Colors.white.withValues(alpha: 0.80)
        : Colors.black.withValues(alpha: 0.60);
    return Semantics(
      button: true,
      selected: selected,
      label: destination.label,
      child: GestureDetector(
        behavior: HitTestBehavior.opaque,
        onTap: () => onSelected(index),
        child: AnimatedContainer(
          duration: const Duration(milliseconds: 240),
          curve: Curves.easeOutCubic,
          margin: const EdgeInsets.symmetric(horizontal: 2),
          decoration: BoxDecoration(
            borderRadius: BorderRadius.circular(26),
            color: selected
                ? (dark
                      ? Colors.white.withValues(alpha: 0.16)
                      : Colors.white.withValues(alpha: 0.96))
                : Colors.transparent,
            border: selected
                ? Border.all(
                    color: dark
                        ? Colors.white.withValues(alpha: 0.22)
                        : Colors.white,
                  )
                : null,
            boxShadow: selected
                ? [
                    BoxShadow(
                      color: colors.primary.withValues(
                        alpha: dark ? 0.26 : 0.14,
                      ),
                      blurRadius: 18,
                      offset: const Offset(0, 6),
                    ),
                  ]
                : null,
          ),
          child: Column(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              IconTheme.merge(
                data: IconThemeData(
                  size: 22,
                  color: selected ? colors.primary : idle,
                ),
                child: selected ? destination.selectedIcon : destination.icon,
              ),
              const SizedBox(height: 2),
              Text(
                destination.label,
                maxLines: 1,
                overflow: TextOverflow.fade,
                softWrap: false,
                style: TextStyle(
                  fontSize: 11,
                  height: 1.1,
                  fontWeight: selected ? FontWeight.w700 : FontWeight.w500,
                  color: selected ? colors.primary : idle,
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}

class GlassTabDestination {
  const GlassTabDestination({
    required this.label,
    required this.icon,
    required this.selectedIcon,
  });

  final String label;
  final Widget icon;
  final Widget selectedIcon;
}
