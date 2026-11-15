import 'dart:ui';

import 'package:flutter/material.dart';

/// Liquid Glass 的基础容器：背景模糊 + 半透明渐变 + 细高光边。
///
/// 主题只能改颜色，模糊必须靠 [BackdropFilter]，所以玻璃效果统一走这个组件，
/// 需要“玻璃感”的地方直接包一层即可。
class GlassPanel extends StatelessWidget {
  const GlassPanel({
    super.key,
    required this.child,
    this.padding = const EdgeInsets.all(16),
    this.blur = 18,
    this.radius = 24,
  });

  final Widget child;
  final EdgeInsetsGeometry padding;
  final double blur;
  final double radius;

  @override
  Widget build(BuildContext context) {
    final dark = Theme.of(context).brightness == Brightness.dark;
    final corners = BorderRadius.circular(radius);
    return ClipRRect(
      borderRadius: corners,
      child: BackdropFilter(
        filter: ImageFilter.blur(sigmaX: blur, sigmaY: blur),
        child: DecoratedBox(
          decoration: BoxDecoration(
            borderRadius: corners,
            border: Border.all(
              color: dark
                  ? Colors.white.withValues(alpha: 0.16)
                  : Colors.white.withValues(alpha: 0.9),
            ),
            gradient: LinearGradient(
              begin: Alignment.topLeft,
              end: Alignment.bottomRight,
              colors: dark
                  ? [
                      Colors.white.withValues(alpha: 0.10),
                      Colors.white.withValues(alpha: 0.03),
                    ]
                  : [
                      Colors.white.withValues(alpha: 0.86),
                      Colors.white.withValues(alpha: 0.56),
                    ],
            ),
          ),
          child: Padding(padding: padding, child: child),
        ),
      ),
    );
  }
}
